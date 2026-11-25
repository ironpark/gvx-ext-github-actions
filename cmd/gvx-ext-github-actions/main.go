package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/github"
	"github.com/ironpark/gvx-ext-github-actions/internal/localact"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/runners"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
	"github.com/ironpark/gvx-ext-github-actions/internal/ui"
)

const version = "0.2.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[gvx-ext-github-actions] ")

	hostAPI := os.Getenv("GVX_HOST_API")
	hostToken := os.Getenv("GVX_HOST_TOKEN")
	dataDir := os.Getenv("GVX_EXT_DATA_DIR")
	dockerHost := os.Getenv("GVX_DOCKER_HOST")

	var host *extsdk.Host
	if hostAPI != "" {
		h, err := extsdk.NewHost(hostAPI, hostToken)
		if err != nil {
			log.Printf("warning: init host client: %v", err)
		} else {
			host = h
		}
	}

	app := extsdk.New().SetVersion(version)
	redactor := mask.New()
	var kv state.KVStore
	if host != nil {
		kv = host
	}
	store := state.NewStore(kv)

	var ctrl *ui.Controller
	notify := func() {
		app.NotifyChanged("main")
		if ctrl != nil {
			go ctrl.UpdateBadge(context.Background())
		}
	}

	localMgr := localact.NewManager(store, redactor, dataDir, dockerHost, notify)
	var runnerBackend runners.HostBackend
	if host != nil {
		runnerBackend = host
	}
	runnerMgr := runners.NewManager(store, runnerBackend, github.NewClient(), redactor, notify)
	ctrl = ui.NewController(store, localMgr, runnerMgr, redactor, notify)
	if host != nil {
		ctrl.SetBadgeHost(host)
	}

	// Bootstrap initial state from Host API (storage, settings, host info, secret keys).
	bootstrapCtx, cancelBoot := context.WithTimeout(context.Background(), 10*time.Second)
	if host != nil {
		if info, err := host.Info(bootstrapCtx); err == nil {
			store.SetHostInfo(info)
		}
		if settings, err := host.Settings(bootstrapCtx); err == nil {
			store.ApplySettings(settings.Raw())
		}
		if keys, err := host.SecretList(bootstrapCtx); err == nil {
			store.SetSecretKeys(keys)
		}
	}
	if err := store.Load(bootstrapCtx); err != nil {
		log.Printf("warning: load persisted state: %v", err)
	}
	cancelBoot()

	ctrl.SyncSelectionDefaults()

	if dockerHost != "" {
		if err := localMgr.Start(); err != nil {
			log.Printf("warning: start local act Docker shim: %v", err)
		}
	}

	ctrl.Register(app)

	app.OnSettingsChanged(func(ctx *extsdk.Context, settings map[string]any) error {
		store.ApplySettings(settings)
		ctrl.SyncSelectionDefaults()
		notify()
		return nil
	})

	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		switch ev.Type {
		case "docker.state", "host.locale", "host.theme":
			if host != nil {
				if info, err := host.Info(ctx); err == nil {
					store.SetHostInfo(info)
				}
			}
			notify()
		case "settings.changed":
			if host != nil {
				if settings, err := host.Settings(ctx); err == nil {
					store.ApplySettings(settings.Raw())
				}
			}
			notify()
		case "sandbox.state":
			notify()
		case "app.stopping":
			runnerMgr.Shutdown(ctx)
			_ = localMgr.Close()
		}
	})

	// Reconcile enabled self-hosted runners after initial state load.
	runnerMgr.Reconcile()
	go ctrl.UpdateBadge(context.Background())

	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runnerMgr.Shutdown(shutCtx)
		_ = localMgr.Close()
		_ = store.PersistHistory(shutCtx)
	}()

	if err := app.Run(context.Background()); err != nil {
		log.Fatalf("extension server exited with error: %v", err)
	}
}
