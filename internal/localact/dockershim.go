package localact

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DockerShim sits locally in front of GVX_DOCKER_HOST to bridge nektos/act's
// expectations with GVX Desktop's strict scoped Docker proxy without weakening
// any host security invariant:
//
//  1. Pre-creates named volumes referenced in POST /containers/create via
//     POST /volumes/create so GVX's scoped Docker proxy stamps io.gvx.extension
//     ownership labels and rejects reserved/foreign volumes.
//  2. Rewrites unsupported HostConfig.NetworkMode ("host", "container:<id>")
//     to a safe bridge/custom network so act jobs and docker:// steps run cleanly.
//  3. Records created container (name, id) pairs so the UI can open an interactive
//     GVX Desktop terminal ("Open Shell") into the live job container.
type DockerShim struct {
	upstream string
	dial     func(ctx context.Context) (net.Conn, error)
	client   *http.Client
	rp       *httputil.ReverseProxy

	mu             sync.Mutex
	ln             net.Listener
	srv            *http.Server
	sockPath       string
	endpoint       string
	onCreate       func(name, containerID string)
	containerNets  map[string]string // container name -> network mode
	closed         bool
}

// StartDockerShim creates and starts a DockerShim forwarding to upstreamHost
// (e.g. "unix:///run/gvx/docker.sock" or "http://10.0.2.2:12345").
func StartDockerShim(upstreamHost string, onCreate func(name, containerID string)) (*DockerShim, error) {
	dialFn, err := dialerForDockerHost(upstreamHost)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialFn(ctx)
		},
		DisableCompression:  true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
	}
	shim := &DockerShim{
		upstream:      upstreamHost,
		dial:          dialFn,
		client:        &http.Client{Transport: tr, Timeout: 30 * time.Second},
		onCreate:      onCreate,
		containerNets: make(map[string]string),
	}
	shim.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			pr.Out.Host = "docker"
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			writeShimDockerErr(w, http.StatusBadGateway, "upstream docker proxy error: "+err.Error())
		},
	}

	var ln net.Listener
	if runtime.GOOS != "windows" {
		sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("gvx-act-%d-%d.sock", os.Getpid(), time.Now().UnixNano()%1_000_000))
		_ = os.Remove(sockPath)
		ln, err = net.Listen("unix", sockPath)
		if err != nil {
			return nil, fmt.Errorf("listen act docker shim socket: %w", err)
		}
		_ = os.Chmod(sockPath, 0o600)
		shim.sockPath = sockPath
		shim.endpoint = "unix://" + sockPath
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen act docker shim tcp: %w", err)
		}
		shim.endpoint = "http://" + ln.Addr().String()
	}

	shim.ln = ln
	shim.srv = &http.Server{
		Handler:           shim,
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		_ = shim.srv.Serve(ln)
	}()
	return shim, nil
}

// DockerHost returns the DOCKER_HOST URI served by this shim.
func (s *DockerShim) DockerHost() string {
	return s.endpoint
}

// Close stops the shim server and removes any temporary socket file.
func (s *DockerShim) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.srv
	ln := s.ln
	sockPath := s.sockPath
	s.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if sockPath != "" {
		_ = os.Remove(sockPath)
	}
	if tr, ok := s.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

func (s *DockerShim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isUpgradeRequest(r) {
		s.proxyHijack(w, r)
		return
	}
	stripped := stripAPIVersion(path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/")))
	if r.Method == http.MethodPost && stripped == "/containers/create" {
		s.handleContainerCreate(w, r)
		return
	}
	s.rp.ServeHTTP(w, r)
}

func (s *DockerShim) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeShimDockerErr(w, http.StatusBadRequest, "read container create body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeShimDockerErr(w, http.StatusBadRequest, "invalid container create JSON: "+err.Error())
		return
	}

	reqName := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("name")), "/")

	// Extract named volumes and normalize NetworkMode in HostConfig.
	var volNames []string
	var effectiveNet string
	if hc, ok := payload["HostConfig"].(map[string]any); ok && hc != nil {
		if binds, ok := hc["Binds"].([]any); ok {
			for _, item := range binds {
				if spec, ok := item.(string); ok {
					if vol, isVol := namedVolumeFromBind(spec); isVol {
						volNames = append(volNames, vol)
					}
				}
			}
		}
		if mounts, ok := hc["Mounts"].([]any); ok {
			for _, item := range mounts {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				mType, _ := m["Type"].(string)
				src, _ := m["Source"].(string)
				if strings.EqualFold(strings.TrimSpace(mType), "volume") && strings.TrimSpace(src) != "" {
					volNames = append(volNames, strings.TrimSpace(src))
				}
			}
		}
		if nm, ok := hc["NetworkMode"].(string); ok {
			trimmed := strings.TrimSpace(nm)
			lower := strings.ToLower(trimmed)
			switch {
			case lower == "host":
				hc["NetworkMode"] = "bridge"
				effectiveNet = "bridge"
			case strings.HasPrefix(lower, "container:"):
				targetCtr := strings.TrimPrefix(trimmed[len("container:"):], "/")
				replacement := s.lookupContainerNet(targetCtr)
				if replacement == "" {
					replacement = "bridge"
				}
				hc["NetworkMode"] = replacement
				effectiveNet = replacement
			default:
				effectiveNet = trimmed
			}
		}
	}

	// Ensure every referenced named volume is explicitly created through the scoped
	// proxy first so GVX stamps io.gvx.extension ownership labels cleanly.
	seenVol := make(map[string]bool, len(volNames))
	for _, vol := range volNames {
		if vol == "" || seenVol[vol] {
			continue
		}
		seenVol[vol] = true
		if err := s.ensureOwnedVolume(r.Context(), vol); err != nil {
			writeShimDockerErr(w, http.StatusForbidden, err.Error())
			return
		}
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		writeShimDockerErr(w, http.StatusInternalServerError, "marshal container create body: "+err.Error())
		return
	}

	upURL := "http://docker" + r.URL.RequestURI()
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(rewritten))
	if err != nil {
		writeShimDockerErr(w, http.StatusBadRequest, err.Error())
		return
	}
	upReq.Header = r.Header.Clone()
	upReq.Header.Set("Content-Type", "application/json")
	upReq.ContentLength = int64(len(rewritten))

	resp, err := s.client.Do(upReq)
	if err != nil {
		writeShimDockerErr(w, http.StatusServiceUnavailable, "docker engine unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		writeShimDockerErr(w, http.StatusBadGateway, err.Error())
		return
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var created struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(respBody, &created) == nil && created.ID != "" {
			s.recordContainerNet(reqName, created.ID, effectiveNet)
			if s.onCreate != nil {
				s.onCreate(reqName, created.ID)
			}
		}
	}

	for k, vals := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *DockerShim) recordContainerNet(name, id, netMode string) {
	if netMode == "" {
		netMode = "bridge"
	}
	s.mu.Lock()
	if name != "" {
		s.containerNets[name] = netMode
	}
	if id != "" {
		s.containerNets[id] = netMode
	}
	s.mu.Unlock()
}

func (s *DockerShim) lookupContainerNet(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.containerNets[ref]
}

func (s *DockerShim) ensureOwnedVolume(ctx context.Context, volName string) error {
	body, err := json.Marshal(map[string]string{"Name": volName})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/volumes/create", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("create volume %q: %w", volName, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("create volume %q (status %d): %s", volName, resp.StatusCode, strings.TrimSpace(string(raw)))
}

// proxyHijack tunnels Docker HTTP Upgrade/Hijack streams (POST /exec/{id}/start,
// POST /containers/{id}/attach) directly between client and upstream GVX_DOCKER_HOST.
func (s *DockerShim) proxyHijack(w http.ResponseWriter, r *http.Request) {
	upConn, err := s.dial(r.Context())
	if err != nil {
		writeShimDockerErr(w, http.StatusServiceUnavailable, "docker engine unreachable: "+err.Error())
		return
	}
	defer upConn.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeShimDockerErr(w, http.StatusBadRequest, "read hijack request body: "+err.Error())
		return
	}
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = ""
	outReq.URL.Host = ""
	outReq.Host = "docker"
	outReq.RequestURI = ""
	outReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	outReq.ContentLength = int64(len(bodyBytes))

	if err := outReq.Write(upConn); err != nil {
		writeShimDockerErr(w, http.StatusBadGateway, "write hijack request upstream: "+err.Error())
		return
	}

	upBR := bufio.NewReader(upConn)
	resp, err := http.ReadResponse(upBR, outReq)
	if err != nil {
		writeShimDockerErr(w, http.StatusBadGateway, "read hijack response upstream: "+err.Error())
		return
	}

	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	rc := http.NewResponseController(w)
	clientConn, clientRW, err := rc.Hijack()
	if err != nil {
		_ = resp.Body.Close()
		writeShimDockerErr(w, http.StatusInternalServerError, "hijack failed: "+err.Error())
		return
	}
	defer clientConn.Close()

	// Write raw HTTP response head directly to hijacked client connection.
	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
	_ = resp.Header.Write(&head)
	head.WriteString("\r\n")
	if _, err := clientConn.Write(head.Bytes()); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, upBR)
		closeWrite(clientConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upConn, clientRW.Reader)
		closeWrite(upConn)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func isUpgradeRequest(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				return true
			}
		}
	}
	return false
}

func namedVolumeFromBind(spec string) (string, bool) {
	src, _, hasColon := strings.Cut(spec, ":")
	if !hasColon || src == "" {
		return "", false
	}
	if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.ContainsAny(src, `/\`) {
		return "", false
	}
	if len(src) == 1 && ((src[0] >= 'a' && src[0] <= 'z') || (src[0] >= 'A' && src[0] <= 'Z')) {
		return "", false
	}
	return strings.TrimSpace(src), true
}

func stripAPIVersion(p string) string {
	rest, ok := strings.CutPrefix(p, "/v")
	if !ok {
		return p
	}
	ver, after, hasSlash := strings.Cut(rest, "/")
	major, minor, ok := strings.Cut(ver, ".")
	if !ok || !allDigits(major) || !allDigits(minor) {
		return p
	}
	if !hasSlash || after == "" {
		return "/"
	}
	return "/" + after
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func dialerForDockerHost(host string) (func(ctx context.Context) (net.Conn, error), error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("GVX_DOCKER_HOST is empty")
	}
	if sock, ok := strings.CutPrefix(host, "unix://"); ok {
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}, nil
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid docker host %q: %w", host, err)
	}
	switch u.Scheme {
	case "http", "tcp":
		addr := u.Host
		if addr == "" {
			return nil, fmt.Errorf("missing host in docker endpoint %q", host)
		}
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q", u.Scheme)
	}
}

func writeShimDockerErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
