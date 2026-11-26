package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/localact"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/runners"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
	"github.com/ironpark/gvx-ext-github-actions/internal/workflows"
)

const (
	TabLocal   = "local"
	TabRunners = "runners"
	TabHistory = "history"

	HistoryFilterAll    = "all"
	HistoryFilterLocal  = "local"
	HistoryFilterRunner = "runner"

	PreviewPipeline = "pipeline"
	PreviewGraph    = "graph"

	AllJobsOption = "All jobs"

	dialogNone      = ""
	dialogSecrets   = "secrets"
	dialogAddRunner = "add_runner"

	maxCodeLogBytes = 22 << 10 // Keep comfortably below uiblock 32 KiB maxLongText
)

// Controller owns the declarative UI state and coordinates Tab 1 (Local act),
// Tab 2 (Self-Hosted Runners), Tab 3 (Unified History), commands, and MCP tools.
type Controller struct {
	store     *state.Store
	localMgr  *localact.Manager
	runnerMgr *runners.Manager
	redactor  *mask.Redactor
	notifyFn  func()
	badgeHost interface {
		SetBadge(ctx context.Context, req extsdk.BadgeRequest) error
	}

	mu sync.Mutex

	activeTab string

	// Tab 1 (Local Workflows) UI selection state.
	selectedWorkspace string
	discovered        map[string][]workflows.WorkflowSummary
	selectedWorkflow  string
	selectedEvent     string
	selectedJob       string
	platformImage     string
	bindWorkdir       bool
	dispatchInputs    map[string]string
	selectedLocalRun  string
	viewingLocalRun   bool
	previewMode       string
	inspectedJobID    string

	// Tab 2 (Self-Hosted Runners) UI state.
	selectedRunnerID string

	// Tab 3 (History) UI state.
	historyFilter      string
	selectedHistoryRun string

	// Active modal dialog (at most one per uiblock rules).
	openDialog        string
	dialogError       string
	newRunnerScope    string
	newRunnerAuthMode string
	newRunnerDocker   bool

	// Last emitted badge text/tone to avoid redundant Host API calls.
	lastBadgeKey string
}

// NewController constructs a UI controller wired to store, localMgr, and runnerMgr.
func NewController(
	store *state.Store,
	localMgr *localact.Manager,
	runnerMgr *runners.Manager,
	redactor *mask.Redactor,
	notifyFn func(),
) *Controller {
	if redactor == nil {
		redactor = mask.New()
	}
	c := &Controller{
		store:             store,
		localMgr:          localMgr,
		runnerMgr:         runnerMgr,
		redactor:          redactor,
		notifyFn:          notifyFn,
		activeTab:         TabLocal,
		discovered:        make(map[string][]workflows.WorkflowSummary),
		dispatchInputs:    make(map[string]string),
		historyFilter:     HistoryFilterAll,
		previewMode:       PreviewPipeline,
		newRunnerScope:    "repo",
		newRunnerAuthMode: "pat",
		platformImage:     store.Settings().DefaultActImage,
	}
	c.SyncSelectionDefaults()
	return c
}

// SetBadgeHost sets the Host API client used to sync sidebar badges asynchronously.
func (c *Controller) SetBadgeHost(h interface {
	SetBadge(ctx context.Context, req extsdk.BadgeRequest) error
}) {
	c.mu.Lock()
	c.badgeHost = h
	c.mu.Unlock()
}

func (c *Controller) resolveLocale(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return normalizeLocale(explicit)
	}
	if c.store != nil {
		if loc := strings.TrimSpace(c.store.HostInfo().Locale); loc != "" {
			return normalizeLocale(loc)
		}
	}
	return LocaleEnglish
}

func tabLabel(loc, tab string) string {
	switch tab {
	case TabRunners:
		return tr(loc, "tab.runners")
	case TabHistory:
		return tr(loc, "tab.history")
	default:
		return tr(loc, "tab.local")
	}
}

func parseTabValue(val string) (string, bool) {
	v := strings.TrimSpace(val)
	switch v {
	case TabLocal, "Local Workflows":
		return TabLocal, true
	case TabRunners, "Self-Hosted Runners":
		return TabRunners, true
	case TabHistory, "History":
		return TabHistory, true
	}
	switch matchLocalizedKey(v, "tab.local", "tab.runners", "tab.history") {
	case "tab.local":
		return TabLocal, true
	case "tab.runners":
		return TabRunners, true
	case "tab.history":
		return TabHistory, true
	}
	return "", false
}

func historyFilterLabel(loc, filter string) string {
	switch filter {
	case HistoryFilterLocal:
		return tr(loc, "history.filter_local")
	case HistoryFilterRunner:
		return tr(loc, "history.filter_runner")
	default:
		return tr(loc, "history.filter_all")
	}
}

func parseHistoryFilterValue(val string) (string, bool) {
	v := strings.TrimSpace(val)
	switch v {
	case HistoryFilterAll, "All":
		return HistoryFilterAll, true
	case HistoryFilterLocal, "Local (act)", "Local":
		return HistoryFilterLocal, true
	case HistoryFilterRunner, "Self-Hosted", "Runner":
		return HistoryFilterRunner, true
	}
	switch matchLocalizedKey(v, "history.filter_all", "history.filter_local", "history.filter_runner") {
	case "history.filter_all":
		return HistoryFilterAll, true
	case "history.filter_local":
		return HistoryFilterLocal, true
	case "history.filter_runner":
		return HistoryFilterRunner, true
	}
	return "", false
}

func parseJobValue(val string) string {
	v := strings.TrimSpace(val)
	if v == "" || v == AllJobsOption || matchLocalizedKey(v, "local.all_jobs") == "local.all_jobs" {
		return AllJobsOption
	}
	return v
}

func previewModeLabel(loc, mode string) string {
	if mode == PreviewGraph {
		return tr(loc, "local.preview_graph")
	}
	return tr(loc, "local.preview_pipeline")
}

func parsePreviewModeValue(val string) (string, bool) {
	v := strings.TrimSpace(val)
	switch v {
	case PreviewPipeline, "Pipeline":
		return PreviewPipeline, true
	case PreviewGraph, "Graph":
		return PreviewGraph, true
	}
	switch matchLocalizedKey(v, "local.preview_pipeline", "local.preview_graph") {
	case "local.preview_pipeline":
		return PreviewPipeline, true
	case "local.preview_graph":
		return PreviewGraph, true
	}
	return "", false
}

func scopeLabel(loc, scope string) string {
	if scope == "org" {
		return tr(loc, "dlg.runner.scope_org")
	}
	return tr(loc, "dlg.runner.scope_repo")
}

func parseScopeValue(val string) string {
	v := strings.TrimSpace(val)
	if v == "org" || matchLocalizedKey(v, "dlg.runner.scope_org") == "dlg.runner.scope_org" {
		return "org"
	}
	return "repo"
}

func authModeLabel(loc, mode string) string {
	if mode == "jit" {
		return tr(loc, "dlg.runner.auth_jit")
	}
	return tr(loc, "dlg.runner.auth_pat")
}

func parseAuthModeValue(val string) string {
	v := strings.TrimSpace(val)
	if v == "jit" || matchLocalizedKey(v, "dlg.runner.auth_jit") == "dlg.runner.auth_jit" {
		return "jit"
	}
	return "pat"
}

func statusLabel(loc, st string) string {
	switch strings.ToLower(strings.TrimSpace(st)) {
	case "running":
		return tr(loc, "status.running")
	case "success", "succeeded":
		return tr(loc, "status.success")
	case "failure", "failed":
		return tr(loc, "status.failure")
	case "cancelled", "canceled":
		return tr(loc, "status.cancelled")
	case "skipped":
		return tr(loc, "status.skipped")
	case "standby":
		return tr(loc, "status.standby")
	case "running_job":
		return tr(loc, "status.running_job")
	case "starting":
		return tr(loc, "status.starting")
	case "cleaning":
		return tr(loc, "status.cleaning")
	case "error":
		return tr(loc, "status.error")
	case "stopped":
		return tr(loc, "status.stopped")
	case "pending":
		return tr(loc, "status.pending")
	default:
		return st
	}
}

// SyncSelectionDefaults ensures selected workspace, workflow, event, runner, and
// history IDs point to valid entries after initial load or refresh.
func (c *Controller) SyncSelectionDefaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncSelectionLocked()
}

func (c *Controller) syncSelectionLocked() {
	if c.platformImage == "" {
		c.platformImage = c.store.Settings().DefaultActImage
	}

	workspaces := c.store.Workspaces()
	if len(workspaces) == 0 {
		c.selectedWorkspace = ""
		c.selectedWorkflow = ""
		c.selectedEvent = ""
		c.selectedJob = AllJobsOption
		c.inspectedJobID = ""
	} else {
		found := false
		for _, w := range workspaces {
			if _, ok := c.discovered[w.Path]; !ok {
				if list, err := workflows.Discover(w.Path); err == nil {
					c.discovered[w.Path] = list
				}
			}
			if w.Path == c.selectedWorkspace {
				found = true
			}
		}
		if !found {
			c.selectedWorkspace = workspaces[0].Path
		}
		c.refreshWorkflowsLocked(c.selectedWorkspace)
	}

	rs := c.store.Runners()
	if len(rs) == 0 {
		c.selectedRunnerID = ""
	} else {
		found := false
		for _, r := range rs {
			if r.ID == c.selectedRunnerID {
				found = true
				break
			}
		}
		if !found {
			c.selectedRunnerID = rs[0].ID
		}
	}

	hist := c.store.History()
	if c.selectedLocalRun == "" {
		for _, h := range hist {
			if h.Kind == "local" {
				c.selectedLocalRun = h.ID
				break
			}
		}
	}
	if c.selectedHistoryRun == "" && len(hist) > 0 {
		c.selectedHistoryRun = hist[0].ID
	}
}

func (c *Controller) refreshWorkflowsLocked(repoPath string) {
	if repoPath == "" {
		return
	}
	list, err := workflows.Discover(repoPath)
	if err == nil {
		c.discovered[repoPath] = list
	}
	wfs := c.discovered[repoPath]
	if len(wfs) == 0 {
		c.selectedWorkflow = ""
		c.selectedEvent = ""
		c.selectedJob = AllJobsOption
		return
	}
	var active *workflows.WorkflowSummary
	for i := range wfs {
		if wfs[i].File == c.selectedWorkflow {
			active = &wfs[i]
			break
		}
	}
	if active == nil {
		active = &wfs[0]
		c.selectedWorkflow = active.File
	}
	if len(active.Events) > 0 {
		if !slices.Contains(active.Events, c.selectedEvent) {
			c.selectedEvent = active.Events[0]
		}
	} else {
		c.selectedEvent = "push"
	}
	if c.selectedJob == "" {
		c.selectedJob = AllJobsOption
	}
	if c.selectedJob != AllJobsOption {
		validJob := false
		for _, j := range active.Jobs {
			if j.ID == c.selectedJob {
				validJob = true
				break
			}
		}
		if !validJob {
			c.selectedJob = AllJobsOption
		}
	}
	if len(active.Jobs) == 0 {
		c.inspectedJobID = ""
	} else {
		validInspected := false
		for _, j := range active.Jobs {
			if j.ID == c.inspectedJobID {
				validInspected = true
				break
			}
		}
		if !validInspected {
			if c.selectedJob != AllJobsOption {
				c.inspectedJobID = c.selectedJob
			} else if stages := computeWorkflowStages(active.Jobs); len(stages) > 0 && len(stages[0].Jobs) > 0 {
				c.inspectedJobID = stages[0].Jobs[0].ID
			} else {
				c.inspectedJobID = active.Jobs[0].ID
			}
		}
	}
	for _, in := range active.DispatchInputs {
		if _, exists := c.dispatchInputs[in.Name]; !exists && in.Default != "" {
			c.dispatchInputs[in.Name] = in.Default
		}
	}
}

// UpdateBadge recalculates the sidebar badge from active local runs & runners.
func (c *Controller) UpdateBadge(ctx context.Context) {
	c.mu.Lock()
	bh := c.badgeHost
	c.mu.Unlock()
	if bh == nil {
		return
	}

	loc := c.resolveLocale("")
	localRunning, runnerSlots, runnerJobs := c.store.ActiveCounts()
	var req extsdk.BadgeRequest
	switch {
	case localRunning+runnerJobs > 0:
		total := localRunning + runnerJobs
		req = extsdk.BadgeRequest{
			Text:    strconv.Itoa(total),
			Tone:    "accent",
			Tooltip: tr(loc, "badge.jobs_running", total),
		}
	case runnerSlots > 0:
		req = extsdk.BadgeRequest{
			Text:    strconv.Itoa(runnerSlots),
			Tone:    "success",
			Tooltip: tr(loc, "badge.runners_standby", runnerSlots),
		}
	default:
		req = extsdk.BadgeRequest{Text: "", Tone: ""}
	}

	key := req.Text + "|" + req.Tone + "|" + req.Tooltip
	c.mu.Lock()
	if c.lastBadgeKey == key {
		c.mu.Unlock()
		return
	}
	c.lastBadgeKey = key
	c.mu.Unlock()

	bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = bh.SetBadge(bctx, req)
}

// Register attaches the "main" view, commands, and MCP tools to app.
func (c *Controller) Register(app *extsdk.App) {
	app.View("main", extsdk.View{
		Title:  "GitHub Actions",
		Chrome: "toolbar",
		Render: c.Render,
		On:     c.Handlers(),
	})

	app.Command("run-local", c.handleCommandRunLocal)
	app.Command("add-runner", c.handleCommandAddRunner)

	app.Tool("list_runners", c.handleToolListRunners)
	app.Tool("run_local_workflow", c.handleToolRunLocalWorkflow)
}

// Render builds the complete declarative extsdk.Node tree for the "main" view.
func (c *Controller) Render(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
	rawLocale := ""
	if ctx != nil {
		rawLocale = ctx.Locale
	}
	loc := c.resolveLocale(rawLocale)

	c.mu.Lock()
	tab := c.activeTab
	dlg := c.openDialog
	c.mu.Unlock()

	children := []any{
		c.renderToolbar(ui, loc, tab),
	}

	switch tab {
	case TabRunners:
		children = append(children, c.renderRunnersTab(ui, loc))
	case TabHistory:
		children = append(children, c.renderHistoryTab(ui, loc))
	default:
		children = append(children, c.renderLocalTab(ui, loc))
	}

	if dlg != dialogNone {
		children = append(children, c.renderDialog(ui, loc, dlg))
	}

	return ui.Column(children...).Fill(true).Gap(8)
}

func (c *Controller) renderToolbar(ui *extsdk.UI, loc, activeTab string) extsdk.Node {
	hi := c.store.HostInfo()
	dockerLabel := tr(loc, "toolbar.docker_off")
	dockerTone := "warning"
	if hi.DockerReady || (c.localMgr != nil && c.localMgr.DockerHost() != "") {
		dockerLabel = tr(loc, "toolbar.docker_ready")
		dockerTone = "success"
	}
	archLabel := strings.ToLower(strings.TrimSpace(hi.Arch))
	if archLabel == "" {
		archLabel = "linux"
	}

	items := []any{
		ui.Tabs(
			tabLabel(loc, activeTab),
			[]string{
				tr(loc, "tab.local"),
				tr(loc, "tab.runners"),
				tr(loc, "tab.history"),
			},
			extsdk.Key("main_tabs"),
			extsdk.Size("small"),
			extsdk.OnChange("set_tab"),
		),
		ui.Pill(dockerLabel, extsdk.Tone(dockerTone), extsdk.Variant("soft")),
		ui.Pill(archLabel, extsdk.Tone("muted"), extsdk.Variant("outline"), extsdk.Icon("cpu")),
	}

	switch activeTab {
	case TabLocal:
		items = append(items,
			ui.Button(tr(loc, "toolbar.secrets", len(c.store.SecretKeys())),
				extsdk.Key("tb_secrets"),
				extsdk.Icon("key"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("open_secrets"),
			),
		)
	case TabRunners:
		items = append(items,
			ui.Button(tr(loc, "toolbar.add_runner"),
				extsdk.Key("tb_add_runner"),
				extsdk.Icon("plus"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("open_add_runner"),
			),
		)
	case TabHistory:
		items = append(items,
			ui.Button(tr(loc, "toolbar.clear_finished"),
				extsdk.Key("tb_clear_history"),
				extsdk.Icon("trash"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.Placeholder(tr(loc, "toolbar.clear_finished_tip")),
				extsdk.OnTap("clear_history"),
			),
		)
	}

	return ui.Toolbar(items...)
}

// --- Tab 1: Local Workflows (nektos/act) ---

func (c *Controller) renderLocalTab(ui *extsdk.UI, loc string) extsdk.Node {
	c.mu.Lock()
	selWS := c.selectedWorkspace
	wfs := slices.Clone(c.discovered[selWS])
	wfCounts := make(map[string]int, len(c.discovered))
	for p, list := range c.discovered {
		wfCounts[p] = len(list)
	}
	selWF := c.selectedWorkflow
	selEvent := c.selectedEvent
	selJob := c.selectedJob
	platImg := c.platformImage
	bindWD := c.bindWorkdir
	dispInputs := make(map[string]string, len(c.dispatchInputs))
	for k, v := range c.dispatchInputs {
		dispInputs[k] = v
	}
	selRunID := c.selectedLocalRun
	viewingRun := c.viewingLocalRun
	prevMode := c.previewMode
	inspectedJob := c.inspectedJobID
	c.mu.Unlock()

	workspaces := c.store.Workspaces()
	hi := c.store.HostInfo()

	var activeWF *workflows.WorkflowSummary
	for i := range wfs {
		if wfs[i].File == selWF {
			activeWF = &wfs[i]
			break
		}
	}

	var leftPane extsdk.Node
	if len(workspaces) == 0 {
		leftPane = c.renderWorkspaceEmptyPane(ui, loc)
	} else {
		leftChildren := []any{
			c.renderWorkspaceCard(ui, loc, workspaces, selWS, wfCounts),
		}
		if selWS != "" {
			leftChildren = append(leftChildren, c.renderWorkflowConfigCard(ui, loc, selWS, wfs, activeWF, selEvent, selJob, platImg, bindWD, dispInputs, hi, selRunID))
		}
		leftPane = ui.Scroll(
			extsdk.Key("local_left_scroll"),
			extsdk.Grow(1),
			ui.Column(leftChildren...).Gap(10),
		)
	}

	// Right pane: Interactive Pipeline / Dependency Graph / Job Inspector, or Live Run Logs.
	var activeRun *state.RunRecord
	if selRunID != "" {
		activeRun, _ = c.store.RunByID(selRunID)
	}
	if activeRun == nil {
		for _, h := range c.store.History() {
			if h.Kind == "local" && (selWS == "" || h.WorkspacePath == selWS) {
				activeRun = h
				break
			}
		}
	}
	if activeRun == nil {
		for _, h := range c.store.History() {
			if h.Kind == "local" {
				activeRun = h
				break
			}
		}
	}
	rightPane := c.renderLocalRunPane(ui, loc, activeRun, viewingRun, activeWF, prevMode, inspectedJob, hi)

	return ui.Split("auto", leftPane, rightPane, extsdk.Key("local_split"), extsdk.Ratio(0.34), extsdk.Grow(1))
}

func (c *Controller) renderWorkspaceEmptyPane(ui *extsdk.UI, loc string) extsdk.Node {
	header := ui.Row(
		ui.Icon("folder", extsdk.Tone("accent")),
		ui.Text(tr(loc, "local.repositories"), extsdk.Weight("semibold"), extsdk.Grow(1)),
		ui.Button("",
			extsdk.Key("btn_refresh_repos_empty"),
			extsdk.Icon("refresh-cw"),
			extsdk.Size("small"),
			extsdk.Variant("ghost"),
			extsdk.Placeholder(tr(loc, "toolbar.refresh")),
			extsdk.OnTap("refresh"),
		),
		ui.Button(tr(loc, "local.add_repo"),
			extsdk.Key("btn_pick_repo"),
			extsdk.Icon("folder-plus"),
			extsdk.Size("small"),
			extsdk.Variant("secondary"),
			extsdk.OnTap("pick_repo"),
		),
	).Gap(6)

	emptyHero := ui.Card(
		extsdk.Key("card_workspaces_empty"),
		extsdk.Variant("soft"),
		extsdk.Gap(8),
		ui.Row(
			ui.Icon("folder-git-2", extsdk.Tone("accent")),
			ui.Text(tr(loc, "local.empty_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
		).Gap(8),
		ui.Text(tr(loc, "local.empty_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Row(
			ui.Button(tr(loc, "local.add_repo_cta"),
				extsdk.Key("btn_pick_repo_cta"),
				extsdk.Icon("folder-plus"),
				extsdk.Variant("primary"),
				extsdk.Size("small"),
				extsdk.OnTap("pick_repo"),
			),
		).Gap(8),
	)

	guideCard := ui.Card(
		extsdk.Key("card_local_guide"),
		extsdk.Variant("outline"),
		extsdk.Gap(6),
		ui.Row(
			ui.Icon("sparkles", extsdk.Tone("muted")),
			ui.Text(tr(loc, "local.quick_start"), extsdk.Weight("medium"), extsdk.Size("small")),
		).Gap(6),
		ui.Text(tr(loc, "local.step1"), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Text(tr(loc, "local.step2"), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Text(tr(loc, "local.step3"), extsdk.Tone("muted"), extsdk.Size("small")),
	)

	return ui.Card(
		extsdk.Key("card_workspaces"),
		extsdk.Grow(1),
		extsdk.Gap(10),
		header,
		ui.Divider(),
		emptyHero,
		guideCard,
	)
}

func (c *Controller) renderWorkspaceCard(ui *extsdk.UI, loc string, workspaces []state.Workspace, selWS string, wfCounts map[string]int) extsdk.Node {
	header := ui.Row(
		ui.Icon("folder", extsdk.Tone("accent")),
		ui.Text(tr(loc, "local.repositories"), extsdk.Weight("semibold"), extsdk.Grow(1)),
		ui.Pill(strconv.Itoa(len(workspaces)), extsdk.Tone("muted"), extsdk.Variant("soft")),
		ui.Button("",
			extsdk.Key("btn_refresh_repos"),
			extsdk.Icon("refresh-cw"),
			extsdk.Size("small"),
			extsdk.Variant("ghost"),
			extsdk.Placeholder(tr(loc, "toolbar.refresh")),
			extsdk.OnTap("refresh"),
		),
		ui.Button(tr(loc, "local.add_repo"),
			extsdk.Key("btn_pick_repo"),
			extsdk.Icon("folder-plus"),
			extsdk.Size("small"),
			extsdk.Variant("secondary"),
			extsdk.OnTap("pick_repo"),
		),
	).Gap(6)

	rows := []any{header, ui.Divider()}
	for i, ws := range workspaces {
		selected := ws.Path == selWS
		cardVar := "outline"
		cardTone := ""
		iconTone := "muted"
		nameWeight := "medium"
		if selected {
			cardVar = "secondary"
			cardTone = "accent"
			iconTone = "accent"
			nameWeight = "semibold"
		}

		titleParts := []any{
			ui.Icon("git-branch", extsdk.Tone(iconTone)),
			ui.Text(mask.TruncateRunes(mask.SanitizeInline(ws.Name), 40), extsdk.Weight(nameWeight), extsdk.Grow(1)),
		}
		if cnt, ok := wfCounts[ws.Path]; ok && cnt > 0 {
			titleParts = append(titleParts, ui.Chip(tr(loc, "local.wf_count", cnt)))
		}

		selectArea := ui.Column(
			extsdk.Key(fmt.Sprintf("ws_sel_%d", i)),
			extsdk.Grow(1),
			extsdk.Gap(2),
			extsdk.OnTap("select_workspace", ws.Path),
			ui.Row(titleParts...).Gap(6),
			ui.Text(mask.TruncateRunes(mask.SanitizeInline(ws.Path), 64), extsdk.Tone("faint"), extsdk.Size("small")),
		)

		rows = append(rows,
			ui.Card(
				extsdk.Key(fmt.Sprintf("ws_%d", i)),
				extsdk.Variant(cardVar),
				extsdk.Tone(cardTone),
				extsdk.Gap(2),
				ui.Row(
					selectArea,
					ui.Button("",
						extsdk.Key(fmt.Sprintf("ws_rm_%d", i)),
						extsdk.Icon("trash"),
						extsdk.Size("small"),
						extsdk.Variant("ghost"),
						extsdk.Placeholder(tr(loc, "local.remove_repo_tip")),
						extsdk.OnTap("remove_workspace", ws.Path),
					),
				).Gap(4),
			),
		)
	}

	args := append([]any{extsdk.Key("card_workspaces"), extsdk.Gap(8)}, rows...)
	return ui.Card(args...)
}

func (c *Controller) renderWorkflowConfigCard(
	ui *extsdk.UI,
	loc string,
	repoPath string,
	wfs []workflows.WorkflowSummary,
	activeWF *workflows.WorkflowSummary,
	selEvent, selJob, platImg string,
	bindWD bool,
	dispInputs map[string]string,
	hi extsdk.HostInfo,
	selRunID string,
) extsdk.Node {
	cfgHeader := []any{
		ui.Icon("file-code", extsdk.Tone("accent")),
		ui.Text(tr(loc, "local.workflow_config"), extsdk.Weight("semibold"), extsdk.Grow(1)),
	}
	if activeWF != nil && activeWF.Name != "" && activeWF.Name != activeWF.File {
		cfgHeader = append(cfgHeader, ui.Chip(mask.TruncateRunes(mask.SanitizeInline(activeWF.Name), 28)))
	}
	items := []any{
		ui.Row(cfgHeader...).Gap(8),
		ui.Divider(),
	}

	if len(wfs) == 0 {
		items = append(items,
			ui.Text(tr(loc, "local.no_workflows", mask.SanitizeInline(filepath.Base(repoPath))), extsdk.Tone("muted"), extsdk.Size("small")),
		)
		args := append([]any{extsdk.Key("card_wf_cfg"), extsdk.Gap(8)}, items...)
		return ui.Card(args...)
	}

	wfOptions := make([]string, 0, len(wfs))
	for _, w := range wfs {
		wfOptions = append(wfOptions, w.File)
	}
	selFile := ""
	if activeWF != nil {
		selFile = activeWF.File
	}

	items = append(items,
		ui.Text(tr(loc, "local.workflow_file"), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Select(wfOptions, selFile, extsdk.Key("wf_select"), extsdk.OnChange("select_workflow")),
	)

	if activeWF != nil && activeWF.ParseError != "" {
		items = append(items,
			ui.Card(
				extsdk.Variant("danger"),
				ui.Text(tr(loc, "local.yaml_error", mask.TruncateRunes(mask.SanitizeInline(activeWF.ParseError), 220)), extsdk.Tone("danger"), extsdk.Size("small")),
			),
		)
		args := append([]any{extsdk.Key("card_wf_cfg"), extsdk.Gap(8)}, items...)
		return ui.Card(args...)
	}

	if activeWF != nil {
		events := activeWF.Events
		if len(events) == 0 {
			events = []string{"push"}
		}
		if !slices.Contains(events, selEvent) {
			selEvent = events[0]
		}

		allJobsLabel := tr(loc, "local.all_jobs")
		jobOpts := []string{allJobsLabel}
		for _, j := range activeWF.Jobs {
			jobOpts = append(jobOpts, j.ID)
		}
		selJobDisplay := selJob
		if selJob == AllJobsOption || !slices.Contains(jobOpts, selJob) {
			selJobDisplay = allJobsLabel
		}

		items = append(items,
			ui.Row(
				ui.Column(
					ui.Text(tr(loc, "local.trigger_event"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Select(events, selEvent, extsdk.Key("event_select"), extsdk.OnChange("select_event")),
				).Gap(4).Grow(1),
				ui.Column(
					ui.Text(tr(loc, "local.job"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Select(jobOpts, selJobDisplay, extsdk.Key("job_select"), extsdk.OnChange("select_job")),
				).Gap(4).Grow(1),
			).Gap(8),
			ui.Text(tr(loc, "local.runner_image"), extsdk.Tone("muted"), extsdk.Size("small")),
			ui.Input(platImg,
				extsdk.Key("platform_image"),
				extsdk.Placeholder(state.DefaultActImage),
				extsdk.OnChange("set_platform_image"),
			),
			ui.Column(
				ui.Toggle(bindWD, tr(loc, "local.bind_workdir_short"),
					extsdk.Key("bind_workdir"),
					extsdk.OnChange("toggle_bind_workdir"),
				),
				ui.Text(tr(loc, "local.bind_workdir_desc"), extsdk.Tone("faint"), extsdk.Size("small")),
			).Gap(2),
		)

		if selEvent == "workflow_dispatch" && len(activeWF.DispatchInputs) > 0 {
			inputNodes := []any{
				ui.Text(tr(loc, "local.dispatch_inputs"), extsdk.Weight("medium"), extsdk.Size("small")),
			}
			for _, in := range activeWF.DispatchInputs {
				val := dispInputs[in.Name]
				if val == "" {
					val = in.Default
				}
				label := in.Name
				if in.Required {
					label += " *"
				}
				inputNodes = append(inputNodes, ui.Text(mask.SanitizeInline(label), extsdk.Tone("muted"), extsdk.Size("small")))
				keyName := sanitizeNodeKey("input_" + in.Name)
				if len(in.Options) > 0 {
					if !slices.Contains(in.Options, val) {
						val = in.Options[0]
					}
					inputNodes = append(inputNodes,
						ui.Select(in.Options, val,
							extsdk.Key(keyName),
							extsdk.OnChange("set_dispatch_input", in.Name),
						),
					)
				} else {
					inputNodes = append(inputNodes,
						ui.Input(val,
							extsdk.Key(keyName),
							extsdk.Placeholder(mask.TruncateRunes(mask.SanitizeInline(in.Description), 120)),
							extsdk.OnChange("set_dispatch_input", in.Name),
						),
					)
				}
			}
			cardArgs := append([]any{extsdk.Key("card_dispatch_inputs"), extsdk.Variant("soft"), extsdk.Gap(6)}, inputNodes...)
			items = append(items, ui.Card(cardArgs...))
		}

		if activeWF.HasAMD64OnlyHint(hi.Arch) {
			items = append(items,
				ui.Card(
					extsdk.Key("card_arm64_hint"),
					extsdk.Variant("soft"),
					ui.Row(
						ui.Icon("info", extsdk.Tone("accent")),
						ui.Column(
							ui.Text(tr(loc, "local.arm64_hint"), extsdk.Tone("muted"), extsdk.Size("small")),
						).Grow(1),
					).Gap(8),
				),
			)
		}

		isRunning := false
		if selRunID != "" {
			if cur, ok := c.store.RunByID(selRunID); ok && cur.Status == "running" {
				isRunning = true
			}
		}

		runBtnLabel := tr(loc, "local.run_workflow")
		if selJob != "" && selJob != AllJobsOption {
			runBtnLabel = tr(loc, "local.run_this_job")
		}
		actionRow := []any{
			ui.Button(runBtnLabel,
				extsdk.Key("btn_run_local"),
				extsdk.Icon("play"),
				extsdk.Size("small"),
				extsdk.Variant("primary"),
				extsdk.Grow(1),
				extsdk.OnTap("run_local"),
			),
			ui.Button(tr(loc, "local.dry_run"),
				extsdk.Key("btn_dryrun_local"),
				extsdk.Icon("eye"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("dryrun_local"),
			),
		}
		if isRunning {
			actionRow = append(actionRow,
				ui.Button(tr(loc, "local.cancel"),
					extsdk.Key("btn_cancel_local"),
					extsdk.Icon("square"),
					extsdk.Size("small"),
					extsdk.Variant("danger"),
					extsdk.OnTap("cancel_local_run", selRunID),
				),
			)
		}
		items = append(items, ui.Row(actionRow...).Gap(8))
	}

	args := append([]any{extsdk.Key("card_wf_cfg"), extsdk.Gap(8)}, items...)
	return ui.Card(args...)
}

func (c *Controller) renderLocalRunPane(
	ui *extsdk.UI,
	loc string,
	run *state.RunRecord,
	viewingRun bool,
	activeWF *workflows.WorkflowSummary,
	previewMode string,
	inspectedJobID string,
	hi extsdk.HostInfo,
) extsdk.Node {
	if (!viewingRun || run == nil) && activeWF != nil && activeWF.ParseError == "" && len(activeWF.Jobs) > 0 {
		return c.renderWorkflowPreviewPane(ui, loc, activeWF, run, previewMode, inspectedJobID)
	}

	if run == nil {
		dockerText := tr(loc, "toolbar.docker_off")
		dockerTone := "warning"
		if hi.DockerReady || (c.localMgr != nil && c.localMgr.DockerHost() != "") {
			dockerText = tr(loc, "toolbar.docker_ready")
			dockerTone = "success"
		}
		archLabel := strings.ToLower(strings.TrimSpace(hi.Arch))
		if archLabel == "" {
			archLabel = "linux"
		}
		defaultImg := c.store.Settings().DefaultActImage
		if defaultImg == "" {
			defaultImg = state.DefaultActImage
		}

		nodes := []any{
			ui.Row(
				ui.Icon("activity", extsdk.Tone("accent")),
				ui.Text(tr(loc, "local.exec_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				ui.Pill(tr(loc, "local.idle"), extsdk.Tone("muted"), extsdk.Variant("soft")),
			).Gap(8),
			ui.Divider(),
			ui.Card(
				extsdk.Key("local_run_ready_card"),
				extsdk.Variant("soft"),
				extsdk.Gap(6),
				ui.Row(
					ui.Icon("terminal", extsdk.Tone("accent")),
					ui.Text(tr(loc, "local.ready_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				).Gap(8),
				ui.Text(tr(loc, "local.ready_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
			),
			ui.Card(
				extsdk.Key("local_env_summary"),
				extsdk.Variant("outline"),
				extsdk.Gap(8),
				ui.Row(
					ui.Icon("cpu", extsdk.Tone("muted")),
					ui.Text(tr(loc, "local.env_title"), extsdk.Weight("medium"), extsdk.Size("small"), extsdk.Grow(1)),
				).Gap(6),
				ui.Row(
					ui.Text(tr(loc, "local.env_engine"), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
					ui.Pill(dockerText, extsdk.Tone(dockerTone), extsdk.Variant("soft")),
				).Gap(8),
				ui.Row(
					ui.Text(tr(loc, "local.env_arch"), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
					ui.Chip(archLabel),
				).Gap(8),
				ui.Row(
					ui.Text(tr(loc, "local.env_default_image"), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
					ui.Chip(mask.TruncateRunes(defaultImg, 42)),
				).Gap(8),
				ui.Row(
					ui.Text(tr(loc, "local.env_secrets"), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
					ui.Text(tr(loc, "local.env_secrets_count", len(c.store.SecretKeys())), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Button(tr(loc, "local.manage_secrets"),
						extsdk.Key("btn_env_secrets"),
						extsdk.Icon("key"),
						extsdk.Size("small"),
						extsdk.Variant("outline"),
						extsdk.OnTap("open_secrets"),
					),
				).Gap(8),
			),
		}

		args := append([]any{extsdk.Key("local_run_empty"), extsdk.Grow(1), extsdk.Gap(10)}, nodes...)
		return ui.Card(args...)
	}

	headerItems := []any{}
	if activeWF != nil && activeWF.ParseError == "" && len(activeWF.Jobs) > 0 {
		headerItems = append(headerItems,
			ui.Button(tr(loc, "local.back_to_workflow"),
				extsdk.Key("btn_back_to_workflow"),
				extsdk.Icon("arrow-left"),
				extsdk.Size("small"),
				extsdk.Variant("ghost"),
				extsdk.OnTap("show_local_preview"),
			),
		)
	}
	headerItems = append(headerItems,
		ui.Icon(statusIcon(run.Status), extsdk.Tone(statusTone(run.Status))),
		ui.Text(mask.TruncateRunes(mask.SanitizeInline(runTitle(run)), 70), extsdk.Weight("semibold"), extsdk.Grow(1)),
		ui.Pill(statusLabel(loc, run.Status), extsdk.Tone(statusTone(run.Status)), extsdk.Variant("soft")),
	)
	if run.Status == "running" {
		headerItems = append(headerItems,
			ui.Button(tr(loc, "local.cancel"),
				extsdk.Key("btn_cancel_local_hdr"),
				extsdk.Icon("square"),
				extsdk.Size("small"),
				extsdk.Variant("danger"),
				extsdk.OnTap("cancel_local_run", run.ID),
			),
		)
	}
	if run.ContainerID != "" {
		headerItems = append(headerItems,
			ui.Button(tr(loc, "local.open_shell"),
				extsdk.Key("btn_open_shell_local"),
				extsdk.Icon("terminal"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("open_shell", "container", run.ContainerID),
			),
		)
	}
	headerItems = append(headerItems,
		ui.Button(tr(loc, "local.copy_logs"),
			extsdk.Key("btn_copy_local_logs"),
			extsdk.Icon("copy"),
			extsdk.Size("small"),
			extsdk.Variant("ghost"),
			extsdk.OnTap("copy_logs", run.ID),
		),
	)

	dagRows := buildDagRows(loc, run.Steps)
	logText := formatLogsForCode(loc, run.Logs, c.redactor)

	colItems := []any{
		ui.Row(headerItems...).Gap(6),
		ui.Text(mask.SanitizeInline(tr(loc, "local.run_meta", run.Event, run.PlatformImage, formatDuration(run.StartedAt, run.FinishedAt))),
			extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Divider(),
	}
	if run.Error != "" {
		colItems = append(colItems,
			ui.Card(
				extsdk.Variant("danger"),
				ui.Text(tr(loc, "local.error_prefix", mask.TruncateRunes(mask.SanitizeInline(run.Error), 240)), extsdk.Tone("danger"), extsdk.Size("small")),
			),
		)
	}
	if len(dagRows) > 0 {
		colItems = append(colItems,
			ui.Dag(dagRows, extsdk.Key("local_run_dag"), extsdk.MaxHeight(180)),
		)
	}
	colItems = append(colItems,
		ui.Text(tr(loc, "local.live_output"), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Scroll(
			extsdk.Key("local_log_scroll"),
			extsdk.Variant("follow"),
			extsdk.Grow(1),
			ui.Code("log", logText),
		),
	)

	args := append([]any{extsdk.Key("local_run_pane"), extsdk.Grow(1), extsdk.Gap(8)}, colItems...)
	return ui.Card(args...)
}

func (c *Controller) renderWorkflowPreviewPane(
	ui *extsdk.UI,
	loc string,
	wf *workflows.WorkflowSummary,
	latestRun *state.RunRecord,
	previewMode string,
	inspectedJobID string,
) extsdk.Node {
	stages := computeWorkflowStages(wf.Jobs)
	totalSteps := 0
	var inspectedJob *workflows.JobSummary
	for i := range wf.Jobs {
		totalSteps += len(wf.Jobs[i].Steps)
		if wf.Jobs[i].ID == inspectedJobID {
			inspectedJob = &wf.Jobs[i]
		}
	}
	if inspectedJob == nil {
		if len(stages) > 0 && len(stages[0].Jobs) > 0 {
			for i := range wf.Jobs {
				if wf.Jobs[i].ID == stages[0].Jobs[0].ID {
					inspectedJob = &wf.Jobs[i]
					break
				}
			}
		}
		if inspectedJob == nil && len(wf.Jobs) > 0 {
			inspectedJob = &wf.Jobs[0]
		}
	}
	activeJobID := ""
	if inspectedJob != nil {
		activeJobID = inspectedJob.ID
	}

	wfTitle := mask.TruncateRunes(mask.SanitizeInline(wf.Name), 48)
	if wfTitle == "" {
		wfTitle = mask.SanitizeInline(wf.File)
	}
	headerParts := []any{
		ui.Icon("workflow", extsdk.Tone("accent")),
		ui.Text(wfTitle, extsdk.Weight("semibold"), extsdk.Grow(1)),
	}
	if latestRun != nil {
		headerParts = append(headerParts,
			ui.Button(tr(loc, "local.view_last_run"),
				extsdk.Key("btn_view_last_run"),
				extsdk.Icon(statusIcon(latestRun.Status)),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("show_local_run"),
			),
		)
	}
	headerParts = append(headerParts,
		ui.Tabs(
			previewModeLabel(loc, previewMode),
			[]string{
				tr(loc, "local.preview_pipeline"),
				tr(loc, "local.preview_graph"),
			},
			extsdk.Key("preview_mode_tabs"),
			extsdk.Size("small"),
			extsdk.OnChange("set_preview_mode"),
		),
	)

	metaItems := []any{}
	if wf.File != "" {
		metaItems = append(metaItems, ui.Chip(mask.TruncateRunes(mask.SanitizeInline(wf.File), 28)))
	}
	metaItems = append(metaItems,
		ui.Pill(tr(loc, "local.jobs_count", len(wf.Jobs)), extsdk.Tone("accent"), extsdk.Variant("soft")),
		ui.Pill(tr(loc, "local.steps_count", totalSteps), extsdk.Tone("muted"), extsdk.Variant("outline")),
	)
	for i, ev := range wf.Events {
		if i >= 4 {
			break
		}
		metaItems = append(metaItems, ui.Chip(mask.TruncateRunes(mask.SanitizeInline(ev), 24)))
	}

	var mainVisual extsdk.Node
	if previewMode == PreviewGraph {
		mainVisual = c.renderWorkflowGraphView(ui, loc, wf, stages, activeJobID)
	} else {
		mainVisual = c.renderWorkflowPipelineView(ui, loc, stages, activeJobID)
	}

	nodes := []any{
		ui.Row(headerParts...).Gap(8),
		ui.Wrap(metaItems...).Gap(6),
		ui.Divider(),
		mainVisual,
	}
	if inspectedJob != nil {
		nodes = append(nodes, c.renderJobInspectorCard(ui, loc, inspectedJob))
	}

	args := append([]any{extsdk.Key("local_run_empty"), extsdk.Grow(1), extsdk.Gap(8)}, nodes...)
	return ui.Card(args...)
}

func (c *Controller) renderWorkflowPipelineView(ui *extsdk.UI, loc string, stages []workflowStage, activeJobID string) extsdk.Node {
	stageNodes := make([]any, 0, len(stages))
	for _, st := range stages {
		stageTone := "muted"
		if st.Level == 0 {
			stageTone = "accent"
		}
		hdrItems := []any{
			ui.Pill(tr(loc, "local.stage_label", st.Level+1), extsdk.Tone(stageTone), extsdk.Variant("soft")),
			ui.Text(tr(loc, "local.jobs_count", len(st.Jobs)), extsdk.Tone("muted"), extsdk.Size("small")),
		}
		if len(st.Deps) > 0 {
			depStr := mask.SanitizeInline(strings.Join(st.Deps, ", "))
			hdrItems = append(hdrItems,
				ui.Text("·", extsdk.Tone("faint"), extsdk.Size("small")),
				ui.Text(mask.TruncateRunes(tr(loc, "local.stage_after", depStr), 72), extsdk.Tone("faint"), extsdk.Size("small"), extsdk.Grow(1)),
			)
		}

		jobCards := make([]any, 0, len(st.Jobs))
		for _, j := range st.Jobs {
			rm := classifyJobRunner(loc, j.RunsOn)
			selected := j.ID == activeJobID
			cardVar := "outline"
			if selected {
				cardVar = "primary"
			}
			iconTone := rm.tone
			if selected {
				iconTone = "accent"
			}

			runsOnText := strings.Join(j.RunsOn, ", ")
			if runsOnText == "" {
				runsOnText = "ubuntu-latest"
			}

			bottomItems := []any{
				ui.Text(mask.TruncateRunes(mask.SanitizeInline(runsOnText), 40), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
			}
			if rm.badge != "" {
				bottomItems = append(bottomItems, ui.Pill(rm.badge, extsdk.Tone(rm.tone), extsdk.Variant("outline")))
			}
			if j.HasService {
				bottomItems = append(bottomItems, ui.Pill(tr(loc, "local.badge_services"), extsdk.Tone("accent"), extsdk.Variant("outline")))
			}

			card := ui.Card(
				extsdk.Key("job_card_"+sanitizeNodeKey(j.ID)),
				extsdk.Variant(cardVar),
				extsdk.Gap(6),
				extsdk.OnTap("inspect_job", j.ID),
				ui.Row(
					ui.Icon(rm.icon, extsdk.Tone(iconTone)),
					ui.Text(mask.TruncateRunes(mask.SanitizeInline(j.Name), 48), extsdk.Weight("semibold"), extsdk.Grow(1)),
					ui.Chip(tr(loc, "local.steps_count", len(j.Steps))),
				).Gap(6),
				ui.Row(bottomItems...).Gap(6),
			)
			jobCards = append(jobCards, card)
		}

		var jobsLayout extsdk.Node
		if len(jobCards) == 1 {
			jobsLayout = jobCards[0].(extsdk.Node)
		} else {
			gridArgs := append([]any{extsdk.Key(fmt.Sprintf("stage_grid_%d", st.Level)), extsdk.Gap(8)}, jobCards...)
			jobsLayout = ui.Grid(2, gridArgs...)
		}

		stageNodes = append(stageNodes,
			ui.Column(
				extsdk.Key(fmt.Sprintf("stage_%d", st.Level)),
				extsdk.Gap(6),
				ui.Row(hdrItems...).Gap(6),
				jobsLayout,
			),
		)
	}

	return ui.Scroll(
		extsdk.Key("pipeline_scroll"),
		extsdk.Grow(1),
		ui.Column(stageNodes...).Gap(12),
	)
}

func (c *Controller) renderWorkflowGraphView(ui *extsdk.UI, loc string, wf *workflows.WorkflowSummary, stages []workflowStage, activeJobID string) extsdk.Node {
	const maxNodes = 100
	const maxEdges = 360

	levelByID := make(map[string]int, len(wf.Jobs))
	for _, st := range stages {
		for _, j := range st.Jobs {
			levelByID[j.ID] = st.Level
		}
	}

	nodes := make([]extsdk.GraphNode, 0, min(len(wf.Jobs), maxNodes))
	nodeExists := make(map[string]bool, len(wf.Jobs))
	for _, st := range stages {
		for _, j := range st.Jobs {
			if len(nodes) >= maxNodes {
				break
			}
			gid := truncateUTF8Bytes(mask.SanitizeInline(j.ID), 100)
			if gid == "" || nodeExists[gid] {
				continue
			}
			nodeExists[gid] = true
			rm := classifyJobRunner(loc, j.RunsOn)
			tone := rm.tone
			if st.Level == 0 {
				tone = "accent"
			}
			runsOn := strings.Join(j.RunsOn, ", ")
			if runsOn == "" {
				runsOn = "ubuntu-latest"
			}
			sub := fmt.Sprintf("%s · %s", runsOn, tr(loc, "local.steps_count", len(j.Steps)))
			nodes = append(nodes, extsdk.GraphNode{
				ID:       gid,
				Label:    truncateUTF8Bytes(mask.SanitizeInline(j.Name), 90),
				Group:    fmt.Sprintf("stage-%d", st.Level+1),
				Tone:     tone,
				Subtitle: truncateUTF8Bytes(mask.SanitizeInline(sub), 120),
				Weight:   float64(max(len(j.Steps), 1)),
			})
		}
	}

	var edges []extsdk.GraphEdge
	for _, j := range wf.Jobs {
		toID := truncateUTF8Bytes(mask.SanitizeInline(j.ID), 100)
		if !nodeExists[toID] {
			continue
		}
		for _, dep := range j.Needs {
			if len(edges) >= maxEdges {
				break
			}
			fromID := truncateUTF8Bytes(mask.SanitizeInline(dep), 100)
			if !nodeExists[fromID] || fromID == toID {
				continue
			}
			edges = append(edges, extsdk.GraphEdge{
				From: fromID,
				To:   toID,
				Tone: "muted",
			})
		}
	}

	selID := truncateUTF8Bytes(mask.SanitizeInline(activeJobID), 100)
	if !nodeExists[selID] {
		selID = ""
	}

	return ui.Graph(nodes, edges,
		extsdk.Key("wf_dep_graph"),
		extsdk.Value(selID),
		extsdk.MaxHeight(680),
		extsdk.Grow(1),
		extsdk.OnChange("inspect_job"),
	)
}

func (c *Controller) renderJobInspectorCard(ui *extsdk.UI, loc string, job *workflows.JobSummary) extsdk.Node {
	rm := classifyJobRunner(loc, job.RunsOn)
	runsOnText := strings.Join(job.RunsOn, ", ")
	if runsOnText == "" {
		runsOnText = "ubuntu-latest"
	}

	hdrParts := []any{
		ui.Icon(rm.icon, extsdk.Tone(rm.tone)),
		ui.Text(mask.TruncateRunes(mask.SanitizeInline(job.Name), 64), extsdk.Weight("semibold"), extsdk.Grow(1)),
		ui.Button(tr(loc, "local.run_this_job"),
			extsdk.Key("btn_run_inspected_job"),
			extsdk.Icon("play"),
			extsdk.Size("small"),
			extsdk.Variant("primary"),
			extsdk.OnTap("run_job_direct", job.ID),
		),
		ui.Button(tr(loc, "local.dry_run"),
			extsdk.Key("btn_dryrun_inspected_job"),
			extsdk.Icon("eye"),
			extsdk.Size("small"),
			extsdk.Variant("outline"),
			extsdk.OnTap("dryrun_job_direct", job.ID),
		),
	}

	metaParts := []any{
		ui.Chip(mask.TruncateRunes(mask.SanitizeInline(runsOnText), 36)),
	}
	if rm.badge != "" {
		metaParts = append(metaParts, ui.Pill(rm.badge, extsdk.Tone(rm.tone), extsdk.Variant("outline")))
	}
	if len(job.Needs) > 0 {
		needsText := mask.SanitizeInline(strings.Join(job.Needs, ", "))
		metaParts = append(metaParts,
			ui.Pill(mask.TruncateRunes(tr(loc, "local.inspector_needs", needsText), 48),
				extsdk.Tone("muted"), extsdk.Variant("outline")),
		)
	}
	if job.HasService {
		metaParts = append(metaParts, ui.Pill(tr(loc, "local.badge_services"), extsdk.Tone("accent"), extsdk.Variant("outline")))
	}

	body := []any{
		ui.Row(hdrParts...).Gap(6),
		ui.Wrap(metaParts...).Gap(6),
	}

	if rm.nonLinux {
		body = append(body,
			ui.Row(
				ui.Icon("info", extsdk.Tone("warning")),
				ui.Column(
					ui.Text(tr(loc, "local.inspector_non_linux", mask.SanitizeInline(runsOnText)), extsdk.Tone("warning"), extsdk.Size("small")),
				).Grow(1),
			).Gap(6),
		)
	}

	body = append(body,
		ui.Divider(),
		ui.Row(
			ui.Icon("list-checks", extsdk.Tone("muted")),
			ui.Text(tr(loc, "local.inspector_steps", len(job.Steps)), extsdk.Weight("medium"), extsdk.Size("small"), extsdk.Grow(1)),
		).Gap(6),
	)

	if len(job.Steps) == 0 {
		body = append(body, ui.Text(tr(loc, "local.inspector_no_steps"), extsdk.Tone("muted"), extsdk.Size("small")))
	} else {
		stepRows := make([]any, 0, len(job.Steps))
		for idx, st := range job.Steps {
			stepRows = append(stepRows,
				ui.Row(
					extsdk.Key(fmt.Sprintf("step_%s_%d", sanitizeNodeKey(job.ID), idx)),
					ui.Chip(fmt.Sprintf("%02d", idx+1)),
					ui.Text(mask.TruncateRunes(mask.SanitizeInline(st), 120), extsdk.Size("small"), extsdk.Grow(1)),
				).Gap(8),
			)
		}
		body = append(body,
			ui.Scroll(
				extsdk.Key("inspector_steps_scroll"),
				extsdk.MaxHeight(140),
				ui.Column(stepRows...).Gap(4),
			),
		)
	}

	args := append([]any{extsdk.Key("card_job_inspector"), extsdk.Variant("soft"), extsdk.Gap(8)}, body...)
	return ui.Card(args...)
}

// --- Tab 2: Self-Hosted Runners (Engine A) ---

func (c *Controller) renderRunnersTab(ui *extsdk.UI, loc string) extsdk.Node {
	c.mu.Lock()
	selID := c.selectedRunnerID
	c.mu.Unlock()

	configs := c.store.Runners()
	allSlots := c.store.AllRunnerSlots()
	hi := c.store.HostInfo()
	isARM64 := strings.EqualFold(hi.Arch, "arm64") || strings.EqualFold(hi.Arch, "aarch64")

	var leftPane extsdk.Node
	if len(configs) == 0 {
		emptyItems := []any{
			ui.Row(
				ui.Icon("server", extsdk.Tone("accent")),
				ui.Text(tr(loc, "runners.title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				ui.Button(tr(loc, "toolbar.add_runner"),
					extsdk.Key("btn_add_runner_hdr"),
					extsdk.Icon("plus"),
					extsdk.Size("small"),
					extsdk.Variant("secondary"),
					extsdk.OnTap("open_add_runner"),
				),
			).Gap(8),
			ui.Divider(),
			ui.Card(
				extsdk.Key("runners_empty"),
				extsdk.Variant("soft"),
				extsdk.Gap(8),
				ui.Row(
					ui.Icon("server", extsdk.Tone("accent")),
					ui.Text(tr(loc, "runners.empty_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				).Gap(8),
				ui.Text(tr(loc, "runners.empty_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
				ui.Row(
					ui.Button(tr(loc, "runners.add_btn"),
						extsdk.Key("btn_add_runner_empty"),
						extsdk.Icon("plus"),
						extsdk.Size("small"),
						extsdk.Variant("primary"),
						extsdk.OnTap("open_add_runner"),
					),
				).Gap(8),
			),
		}
		if isARM64 {
			emptyItems = append(emptyItems,
				ui.Card(
					extsdk.Key("runner_arm64_banner"),
					extsdk.Variant("outline"),
					ui.Row(
						ui.Icon("cpu", extsdk.Tone("accent")),
						ui.Column(
							ui.Text(tr(loc, "runners.arm64_banner"), extsdk.Tone("muted"), extsdk.Size("small")),
						).Grow(1),
					).Gap(8),
				),
			)
		}
		args := append([]any{extsdk.Key("runners_left_empty"), extsdk.Grow(1), extsdk.Gap(10)}, emptyItems...)
		leftPane = ui.Card(args...)
	} else {
		leftItems := []any{}
		if isARM64 {
			leftItems = append(leftItems,
				ui.Card(
					extsdk.Key("runner_arm64_banner"),
					extsdk.Variant("soft"),
					ui.Row(
						ui.Icon("cpu", extsdk.Tone("accent")),
						ui.Column(
							ui.Text(tr(loc, "runners.arm64_banner"), extsdk.Tone("muted"), extsdk.Size("small")),
						).Grow(1),
					).Gap(8),
				),
			)
		}
		for _, cfg := range configs {
			slots := allSlots[cfg.ID]
			selected := cfg.ID == selID
			standby, busy, errCnt := 0, 0, 0
			for _, sl := range slots {
				switch sl.Status {
				case "standby":
					standby++
				case "running_job":
					busy++
				case "error":
					errCnt++
				}
			}

			statusText := tr(loc, "runners.status_stopped")
			statusT := "muted"
			if cfg.Enabled {
				switch {
				case busy > 0:
					statusText = tr(loc, "runners.status_running", busy, cfg.Concurrency)
					statusT = "accent"
				case standby > 0:
					statusText = tr(loc, "runners.status_standby", standby, cfg.Concurrency)
					statusT = "success"
				case errCnt > 0:
					statusText = tr(loc, "runners.status_error")
					statusT = "danger"
				default:
					statusText = tr(loc, "runners.status_starting")
					statusT = "warning"
				}
			}

			cardVariant := "outline"
			cardTone := ""
			if selected {
				cardVariant = "secondary"
				cardTone = "accent"
			}

			toggleLabel := tr(loc, "runners.start")
			toggleIcon := "play"
			toggleAction := "start_runner"
			toggleVar := "primary"
			if cfg.Enabled {
				toggleLabel = tr(loc, "runners.stop")
				toggleIcon = "square"
				toggleAction = "stop_runner"
				toggleVar = "outline"
			}

			titleParts := []any{
				ui.Icon("server", extsdk.Tone(statusT)),
				ui.Text(mask.TruncateRunes(mask.SanitizeInline(cfg.Name), 36), extsdk.Weight("semibold"), extsdk.Grow(1)),
				ui.Pill(statusText, extsdk.Tone(statusT), extsdk.Variant("soft")),
			}
			if cfg.AllowDocker {
				titleParts = append(titleParts, ui.Pill("Docker", extsdk.Tone("warning"), extsdk.Variant("outline")))
			}

			leftItems = append(leftItems,
				ui.Card(
					extsdk.Key("runner_card_"+cfg.ID),
					extsdk.Variant(cardVariant),
					extsdk.Tone(cardTone),
					extsdk.Gap(8),
					ui.Column(
						extsdk.Key("runner_sel_"+cfg.ID),
						extsdk.Gap(4),
						extsdk.OnTap("select_runner", cfg.ID),
						ui.Row(titleParts...).Gap(6),
						ui.Text(mask.SanitizeInline(tr(loc, "runners.meta_line",
							scopeLabel(loc, cfg.Scope), cfg.Target, cfg.Concurrency, strings.Join(cfg.Labels, ", "))),
							extsdk.Tone("muted"), extsdk.Size("small")),
					),
					ui.Row(
						ui.Button(toggleLabel,
							extsdk.Key("runner_toggle_"+cfg.ID),
							extsdk.Icon(toggleIcon),
							extsdk.Size("small"),
							extsdk.Variant(toggleVar),
							extsdk.OnTap(toggleAction, cfg.ID),
						),
						ui.Button(tr(loc, "runners.delete"),
							extsdk.Key("runner_del_"+cfg.ID),
							extsdk.Icon("trash"),
							extsdk.Size("small"),
							extsdk.Variant("ghost"),
							extsdk.Tone("danger"),
							extsdk.OnTap("delete_runner", cfg.ID),
						),
					).Gap(6),
				),
			)
		}
		leftPane = ui.Scroll(
			extsdk.Key("runners_left_scroll"),
			extsdk.Grow(1),
			ui.Column(leftItems...).Gap(8),
		)
	}

	var selCfg *state.RunnerConfig
	for i := range configs {
		if configs[i].ID == selID {
			selCfg = &configs[i]
			break
		}
	}
	rightPane := c.renderRunnerDetailPane(ui, loc, selCfg, allSlots)

	return ui.Split("auto", leftPane, rightPane, extsdk.Key("runners_split"), extsdk.Ratio(0.36), extsdk.Grow(1))
}

func (c *Controller) renderRunnerDetailPane(ui *extsdk.UI, loc string, cfg *state.RunnerConfig, allSlots map[string][]state.RunnerLiveSlot) extsdk.Node {
	if cfg == nil {
		return ui.Card(
			extsdk.Key("runner_detail_empty"),
			extsdk.Grow(1),
			extsdk.Gap(10),
			ui.Row(
				ui.Icon("server", extsdk.Tone("accent")),
				ui.Text(tr(loc, "runners.detail_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				ui.Pill(tr(loc, "local.idle"), extsdk.Tone("muted"), extsdk.Variant("soft")),
			).Gap(8),
			ui.Divider(),
			ui.Card(
				extsdk.Key("runner_detail_empty_card"),
				extsdk.Variant("soft"),
				extsdk.Gap(6),
				ui.Row(
					ui.Icon("shield-check", extsdk.Tone("accent")),
					ui.Text(tr(loc, "runners.detail_empty_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				).Gap(8),
				ui.Text(tr(loc, "runners.detail_empty_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
			),
		)
	}

	slots := allSlots[cfg.ID]
	dockerFlag := tr(loc, "runners.bool_off")
	if cfg.AllowDocker {
		dockerFlag = tr(loc, "runners.bool_on")
	}
	items := []any{
		ui.Row(
			ui.Icon("server", extsdk.Tone("accent")),
			ui.Text(mask.SanitizeInline(cfg.Name), extsdk.Weight("semibold"), extsdk.Grow(1)),
			ui.Pill(scopeLabel(loc, cfg.Scope)+": "+cfg.Target, extsdk.Tone("muted"), extsdk.Variant("outline")),
		).Gap(8),
		ui.Text(mask.SanitizeInline(tr(loc, "runners.detail_meta",
			cfg.Image, strings.ToUpper(cfg.AuthMode), dockerFlag)),
			extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Divider(),
		ui.Text(tr(loc, "runners.slots_heading"), extsdk.Weight("medium"), extsdk.Size("small")),
	}

	if len(slots) == 0 {
		items = append(items, ui.Text(tr(loc, "runners.pool_stopped"), extsdk.Tone("muted"), extsdk.Size("small")))
	} else {
		for _, sl := range slots {
			slotTone := "muted"
			slotIcon := "clock"
			switch sl.Status {
			case "standby":
				slotTone = "success"
				slotIcon = "circle-check"
			case "running_job":
				slotTone = "accent"
				slotIcon = "activity"
			case "error":
				slotTone = "danger"
				slotIcon = "circle-x"
			case "starting", "cleaning":
				slotTone = "warning"
				slotIcon = "clock"
			}
			desc := tr(loc, "runners.slot_desc", sl.SlotIndex, statusLabel(loc, sl.Status))
			if sl.JobName != "" {
				desc += tr(loc, "runners.slot_job", sl.JobName)
			}
			if sl.Name != "" {
				desc += " (" + sl.Name + ")"
			}
			rowItems := []any{
				ui.Icon(slotIcon, extsdk.Tone(slotTone)),
				ui.Text(mask.TruncateRunes(mask.SanitizeInline(desc), 100), extsdk.Grow(1)),
				ui.Pill(statusLabel(loc, sl.Status), extsdk.Tone(slotTone), extsdk.Variant("soft")),
			}
			if sl.SandboxID != "" {
				rowItems = append(rowItems,
					ui.Button(tr(loc, "local.open_shell"),
						extsdk.Key(fmt.Sprintf("slot_shell_%s_%d", cfg.ID, sl.SlotIndex)),
						extsdk.Icon("terminal"),
						extsdk.Size("small"),
						extsdk.Variant("outline"),
						extsdk.OnTap("open_shell", "sandbox", sl.SandboxID),
					),
				)
			}
			slotNodes := []any{ui.Row(rowItems...).Gap(6)}
			if sl.LastError != "" {
				slotNodes = append(slotNodes,
					ui.Text(mask.TruncateRunes(mask.SanitizeInline(sl.LastError), 220), extsdk.Tone("danger"), extsdk.Size("small")),
				)
			}
			slotCardArgs := append([]any{extsdk.Key(fmt.Sprintf("slot_%s_%d", cfg.ID, sl.SlotIndex)), extsdk.Variant("soft"), extsdk.Gap(4)}, slotNodes...)
			items = append(items, ui.Card(slotCardArgs...))
		}
	}

	// Find most recent run executed by this runner pool.
	var latestRun *state.RunRecord
	for _, h := range c.store.History() {
		if h.Kind == "runner" && h.RunnerID == cfg.ID {
			latestRun = h
			break
		}
	}
	if latestRun != nil {
		items = append(items,
			ui.Divider(),
			ui.Row(
				ui.Text(tr(loc, "runners.latest_logs", mask.TruncateRunes(mask.SanitizeInline(latestRun.JobID), 50)), extsdk.Weight("medium"), extsdk.Grow(1)),
				ui.Pill(statusLabel(loc, latestRun.Status), extsdk.Tone(statusTone(latestRun.Status)), extsdk.Variant("soft")),
				ui.Button(tr(loc, "runners.reproduce"),
					extsdk.Key("runner_repro_"+latestRun.ID),
					extsdk.Icon("rotate-ccw"),
					extsdk.Size("small"),
					extsdk.Variant("outline"),
					extsdk.OnTap("reproduce_locally", latestRun.ID),
				),
			).Gap(6),
			ui.Scroll(
				extsdk.Key("runner_log_scroll"),
				extsdk.Variant("follow"),
				extsdk.Grow(1),
				ui.Code("log", formatLogsForCode(loc, latestRun.Logs, c.redactor)),
			),
		)
	}

	args := append([]any{extsdk.Key("runner_detail_pane"), extsdk.Grow(1), extsdk.Gap(8)}, items...)
	return ui.Card(args...)
}

// --- Tab 3: Unified History ---

func (c *Controller) renderHistoryTab(ui *extsdk.UI, loc string) extsdk.Node {
	c.mu.Lock()
	filter := c.historyFilter
	selID := c.selectedHistoryRun
	c.mu.Unlock()

	all := c.store.History()
	filtered := make([]*state.RunRecord, 0, len(all))
	for _, r := range all {
		switch filter {
		case HistoryFilterLocal:
			if r.Kind != "local" {
				continue
			}
		case HistoryFilterRunner:
			if r.Kind != "runner" {
				continue
			}
		}
		filtered = append(filtered, r)
	}

	var selected *state.RunRecord
	for _, r := range filtered {
		if r.ID == selID {
			selected = r
			break
		}
	}
	if selected == nil && len(filtered) > 0 {
		selected = filtered[0]
	}

	headerRow := ui.Row(
		ui.Icon("clock", extsdk.Tone("accent")),
		ui.Text(tr(loc, "history.title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
	).Gap(8)

	filterTabs := ui.Tabs(
		historyFilterLabel(loc, filter),
		[]string{
			tr(loc, "history.filter_all"),
			tr(loc, "history.filter_local"),
			tr(loc, "history.filter_runner"),
		},
		extsdk.Key("history_filter_tabs"),
		extsdk.Size("small"),
		extsdk.OnChange("set_history_filter"),
	)

	var leftBody extsdk.Node
	if len(filtered) == 0 {
		leftBody = ui.Card(
			extsdk.Key("history_empty_card"),
			extsdk.Variant("soft"),
			extsdk.Gap(6),
			ui.Text(tr(loc, "history.empty_title"), extsdk.Weight("semibold")),
			ui.Text(tr(loc, "history.empty_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
		)
	} else {
		runRows := make([]any, 0, len(filtered))
		for _, r := range filtered {
			kindIcon := "workflow"
			kindLabel := tr(loc, "history.kind_local")
			if r.Kind == "runner" {
				kindIcon = "server"
				kindLabel = tr(loc, "history.kind_runner")
			}
			cardVar := "outline"
			cardTone := ""
			if selected != nil && selected.ID == r.ID {
				cardVar = "secondary"
				cardTone = "accent"
			}
			sub := fmt.Sprintf("%s · %s", r.StartedAt.Local().Format("15:04:05"), formatDuration(r.StartedAt, r.FinishedAt))
			if r.Target != "" {
				sub += " · " + r.Target
			} else if r.WorkspacePath != "" {
				sub += " · " + filepath.Base(r.WorkspacePath)
			}
			runRows = append(runRows,
				ui.Card(
					extsdk.Key("hist_item_"+r.ID),
					extsdk.Variant(cardVar),
					extsdk.Tone(cardTone),
					extsdk.Gap(4),
					extsdk.OnTap("select_history_run", r.ID),
					ui.Row(
						ui.Icon(kindIcon, extsdk.Tone(statusTone(r.Status))),
						ui.Text(mask.TruncateRunes(mask.SanitizeInline(runTitle(r)), 44), extsdk.Weight("medium"), extsdk.Grow(1)),
						ui.Pill(statusLabel(loc, r.Status), extsdk.Tone(statusTone(r.Status)), extsdk.Variant("soft")),
					).Gap(6),
					ui.Row(
						ui.Text(mask.TruncateRunes(mask.SanitizeInline(sub), 60), extsdk.Tone("muted"), extsdk.Size("small"), extsdk.Grow(1)),
						ui.Chip(kindLabel),
					).Gap(6),
				),
			)
		}
		leftBody = ui.Scroll(
			extsdk.Key("history_left_scroll"),
			extsdk.Grow(1),
			ui.Column(runRows...).Gap(6),
		)
	}

	leftPane := ui.Card(
		extsdk.Key("history_left_pane"),
		extsdk.Grow(1),
		extsdk.Gap(10),
		headerRow,
		filterTabs,
		ui.Divider(),
		leftBody,
	)

	rightPane := c.renderHistoryDetailPane(ui, loc, selected)
	return ui.Split("auto", leftPane, rightPane, extsdk.Key("history_split"), extsdk.Ratio(0.36), extsdk.Grow(1))
}

func (c *Controller) renderHistoryDetailPane(ui *extsdk.UI, loc string, run *state.RunRecord) extsdk.Node {
	if run == nil {
		return ui.Card(
			extsdk.Key("history_detail_empty"),
			extsdk.Grow(1),
			extsdk.Gap(10),
			ui.Row(
				ui.Icon("file-text", extsdk.Tone("accent")),
				ui.Text(tr(loc, "history.detail_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				ui.Pill(tr(loc, "local.idle"), extsdk.Tone("muted"), extsdk.Variant("soft")),
			).Gap(8),
			ui.Divider(),
			ui.Card(
				extsdk.Key("history_detail_empty_card"),
				extsdk.Variant("soft"),
				extsdk.Gap(6),
				ui.Row(
					ui.Icon("clock", extsdk.Tone("accent")),
					ui.Text(tr(loc, "history.detail_empty_title"), extsdk.Weight("semibold"), extsdk.Grow(1)),
				).Gap(8),
				ui.Text(tr(loc, "history.detail_empty_desc"), extsdk.Tone("muted"), extsdk.Size("small")),
			),
		)
	}

	header := []any{
		ui.Icon(statusIcon(run.Status), extsdk.Tone(statusTone(run.Status))),
		ui.Text(mask.TruncateRunes(mask.SanitizeInline(runTitle(run)), 64), extsdk.Weight("semibold"), extsdk.Grow(1)),
		ui.Pill(statusLabel(loc, run.Status), extsdk.Tone(statusTone(run.Status)), extsdk.Variant("soft")),
		ui.Button(tr(loc, "history.reproduce_act"),
			extsdk.Key("btn_repro_local"),
			extsdk.Icon("rotate-ccw"),
			extsdk.Size("small"),
			extsdk.Variant("primary"),
			extsdk.OnTap("reproduce_locally", run.ID),
		),
	}
	if run.ContainerID != "" && run.Status == "running" {
		header = append(header,
			ui.Button(tr(loc, "local.open_shell"),
				extsdk.Key("btn_hist_shell"),
				extsdk.Icon("terminal"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("open_shell", "container", run.ContainerID),
			),
		)
	} else if run.SandboxID != "" && run.Status == "running" {
		header = append(header,
			ui.Button(tr(loc, "local.open_shell"),
				extsdk.Key("btn_hist_shell"),
				extsdk.Icon("terminal"),
				extsdk.Size("small"),
				extsdk.Variant("outline"),
				extsdk.OnTap("open_shell", "sandbox", run.SandboxID),
			),
		)
	}
	header = append(header,
		ui.Button(tr(loc, "local.copy_logs"),
			extsdk.Key("btn_hist_copy"),
			extsdk.Icon("copy"),
			extsdk.Size("small"),
			extsdk.Variant("ghost"),
			extsdk.OnTap("copy_logs", run.ID),
		),
	)

	kindLabel := tr(loc, "history.kind_local")
	if run.Kind == "runner" {
		kindLabel = tr(loc, "history.kind_runner")
	}
	meta := tr(loc, "history.meta", kindLabel, run.StartedAt.Local().Format("15:04:05"), formatDuration(run.StartedAt, run.FinishedAt))
	if run.Target != "" {
		meta += tr(loc, "history.meta_target", run.Target)
	} else if run.WorkspacePath != "" {
		meta += tr(loc, "history.meta_repo", filepath.Base(run.WorkspacePath))
	}

	items := []any{
		ui.Row(header...).Gap(6),
		ui.Text(mask.TruncateRunes(mask.SanitizeInline(meta), 160), extsdk.Tone("muted"), extsdk.Size("small")),
		ui.Divider(),
	}
	if run.Error != "" {
		items = append(items,
			ui.Card(
				extsdk.Variant("danger"),
				ui.Text(tr(loc, "local.error_prefix", mask.TruncateRunes(mask.SanitizeInline(run.Error), 240)), extsdk.Tone("danger"), extsdk.Size("small")),
			),
		)
	}
	if dagRows := buildDagRows(loc, run.Steps); len(dagRows) > 0 {
		items = append(items, ui.Dag(dagRows, extsdk.Key("history_dag"), extsdk.MaxHeight(180)))
	}
	items = append(items,
		ui.Scroll(
			extsdk.Key("history_log_scroll"),
			extsdk.Variant("follow"),
			extsdk.Grow(1),
			ui.Code("log", formatLogsForCode(loc, run.Logs, c.redactor)),
		),
	)

	args := append([]any{extsdk.Key("history_detail_pane"), extsdk.Grow(1), extsdk.Gap(8)}, items...)
	return ui.Card(args...)
}

// --- Modal Dialogs ---

func (c *Controller) renderDialog(ui *extsdk.UI, loc, dlg string) extsdk.Node {
	c.mu.Lock()
	errMsg := c.dialogError
	scope := c.newRunnerScope
	authMode := c.newRunnerAuthMode
	allowDocker := c.newRunnerDocker
	c.mu.Unlock()

	switch dlg {
	case dialogSecrets:
		keys := c.store.SecretKeys()
		rows := []any{
			ui.Text(tr(loc, "dlg.secrets.desc"), extsdk.Tone("muted"), extsdk.Size("small")),
		}
		if errMsg != "" {
			rows = append(rows, ui.Text(mask.SanitizeInline(errMsg), extsdk.Tone("danger"), extsdk.Size("small")))
		}
		rows = append(rows,
			ui.Text(tr(loc, "dlg.secrets.add_heading"), extsdk.Weight("medium"), extsdk.Size("small")),
			ui.Input("", extsdk.Key("secret_name"), extsdk.Placeholder(tr(loc, "dlg.secrets.name_ph"))),
			ui.Input("", extsdk.Key("secret_value"), extsdk.Variant("password"), extsdk.Placeholder(tr(loc, "dlg.secrets.val_ph"))),
			ui.Row(
				ui.Button(tr(loc, "dlg.secrets.save"), extsdk.Key("btn_save_secret"), extsdk.Icon("lock"), extsdk.Size("small"), extsdk.Variant("primary"), extsdk.OnTap("save_secret")),
				ui.Button(tr(loc, "dlg.secrets.close"), extsdk.Key("btn_close_secrets"), extsdk.Size("small"), extsdk.Variant("ghost"), extsdk.OnTap("close_dialog")),
			).Gap(8),
			ui.Divider(),
			ui.Text(tr(loc, "dlg.secrets.stored_heading", len(keys)), extsdk.Weight("medium"), extsdk.Size("small")),
		)
		if len(keys) == 0 {
			rows = append(rows, ui.Text(tr(loc, "dlg.secrets.empty"), extsdk.Tone("muted"), extsdk.Size("small")))
		} else {
			for i, k := range keys {
				rows = append(rows,
					ui.Row(
						extsdk.Key(fmt.Sprintf("sec_row_%d", i)),
						ui.Icon("key", extsdk.Tone("muted")),
						ui.Text(mask.SanitizeInline(k), extsdk.Grow(1)),
						ui.Pill(tr(loc, "dlg.secrets.pill_set"), extsdk.Tone("success"), extsdk.Variant("soft")),
						ui.Button("",
							extsdk.Key(fmt.Sprintf("sec_del_%d", i)),
							extsdk.Icon("trash"),
							extsdk.Size("small"),
							extsdk.Variant("ghost"),
							extsdk.Tone("danger"),
							extsdk.OnTap("delete_secret", k),
						),
					).Gap(6),
				)
			}
		}
		return ui.Dialog(tr(loc, "dlg.secrets.title"),
			extsdk.Key("dlg_secrets"),
			extsdk.OnChange("close_dialog"),
			ui.Column(rows...).Gap(6),
		)

	case dialogAddRunner:
		defaultLabels := strings.Join(state.DefaultArchLabels(c.store.HostInfo().Arch), ",")
		defaultConc := strconv.Itoa(max(c.store.Settings().DefaultConcurrency, 1))
		hasDefaultPAT := c.store.Settings().HasDefaultPAT

		tokenPlaceholder := tr(loc, "dlg.runner.ph_pat")
		if authMode == "jit" {
			tokenPlaceholder = tr(loc, "dlg.runner.ph_jit")
		} else if hasDefaultPAT {
			tokenPlaceholder = tr(loc, "dlg.runner.ph_default_pat")
		}

		rows := []any{
			ui.Text(tr(loc, "dlg.runner.desc"), extsdk.Tone("muted"), extsdk.Size("small")),
		}
		if errMsg != "" {
			rows = append(rows, ui.Text(mask.SanitizeInline(errMsg), extsdk.Tone("danger"), extsdk.Size("small")))
		}
		rows = append(rows,
			ui.Row(
				ui.Column(
					ui.Text(tr(loc, "dlg.runner.scope"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Select(
						[]string{tr(loc, "dlg.runner.scope_repo"), tr(loc, "dlg.runner.scope_org")},
						scopeLabel(loc, scope),
						extsdk.Key("new_runner_scope"),
						extsdk.OnChange("set_new_runner_scope"),
					),
				).Gap(4).Grow(1),
				ui.Column(
					ui.Text(tr(loc, "dlg.runner.auth_mode"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Select(
						[]string{tr(loc, "dlg.runner.auth_pat"), tr(loc, "dlg.runner.auth_jit")},
						authModeLabel(loc, authMode),
						extsdk.Key("new_runner_auth"),
						extsdk.OnChange("set_new_runner_auth"),
					),
				).Gap(4).Grow(1),
			).Gap(8),
			ui.Text(tr(loc, "dlg.runner.target"), extsdk.Tone("muted"), extsdk.Size("small")),
			ui.Input("", extsdk.Key("new_runner_target"), extsdk.Placeholder("owner/repo")),
			ui.Text(tr(loc, "dlg.runner.name_prefix"), extsdk.Tone("muted"), extsdk.Size("small")),
			ui.Input("", extsdk.Key("new_runner_name"), extsdk.Placeholder("gvx-runner")),
			ui.Text(tr(loc, "dlg.runner.credential"), extsdk.Tone("muted"), extsdk.Size("small")),
			ui.Input("", extsdk.Key("new_runner_token"), extsdk.Variant("password"), extsdk.Placeholder(tokenPlaceholder)),
			ui.Text(tr(loc, "dlg.runner.labels"), extsdk.Tone("muted"), extsdk.Size("small")),
			ui.Input(defaultLabels, extsdk.Key("new_runner_labels"), extsdk.Placeholder(defaultLabels)),
			ui.Row(
				ui.Column(
					ui.Text(tr(loc, "dlg.runner.image"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Input(state.DefaultRunnerImage, extsdk.Key("new_runner_image"), extsdk.Placeholder(state.DefaultRunnerImage)),
				).Gap(4).Grow(3),
				ui.Column(
					ui.Text(tr(loc, "dlg.runner.concurrency"), extsdk.Tone("muted"), extsdk.Size("small")),
					ui.Input(defaultConc, extsdk.Key("new_runner_concurrency"), extsdk.Placeholder("1")),
				).Gap(4).Grow(1),
			).Gap(8),
			ui.Toggle(allowDocker, tr(loc, "dlg.runner.allow_docker"),
				extsdk.Key("new_runner_docker"),
				extsdk.OnChange("toggle_new_runner_docker"),
			),
			ui.Row(
				ui.Button(tr(loc, "dlg.runner.create"), extsdk.Key("btn_create_runner"), extsdk.Icon("plus"), extsdk.Size("small"), extsdk.Variant("primary"), extsdk.OnTap("create_runner")),
				ui.Button(tr(loc, "local.cancel"), extsdk.Key("btn_cancel_runner"), extsdk.Size("small"), extsdk.Variant("ghost"), extsdk.OnTap("close_dialog")),
			).Gap(8),
		)

		return ui.Dialog(tr(loc, "dlg.runner.title"),
			extsdk.Key("dlg_add_runner"),
			extsdk.OnChange("close_dialog"),
			ui.Column(rows...).Gap(6),
		)
	}

	return ui.Dialog("Dialog", extsdk.Key("dlg_empty"), extsdk.OnChange("close_dialog"))
}

// --- Event Handlers ---

func (c *Controller) Handlers() extsdk.Handlers {
	return extsdk.Handlers{
		"set_tab": func(ctx *extsdk.EventContext, val string) error {
			if tab, ok := parseTabValue(val); ok {
				c.mu.Lock()
				c.activeTab = tab
				c.mu.Unlock()
				c.notify(ctx)
			}
			return nil
		},

		"refresh": func(ctx *extsdk.EventContext) error {
			c.refreshHostState(ctx)
			c.SyncSelectionDefaults()
			c.notify(ctx)
			return nil
		},

		// --- Tab 1 handlers ---
		"pick_repo": func(ctx *extsdk.EventContext) error {
			loc := c.resolveLocale(ctx.Locale)
			dir, ok, err := ctx.Host.PickFolder(ctx)
			if err != nil {
				_ = ctx.Host.Toast(ctx, "error", tr(loc, "toast.pick_folder_failed", err.Error()))
				return err
			}
			if !ok || strings.TrimSpace(dir) == "" {
				return nil
			}
			ws, err := c.store.AddWorkspace(ctx, dir)
			if err != nil {
				_ = ctx.Host.Toast(ctx, "error", tr(loc, "toast.save_repo_failed", err.Error()))
				return err
			}
			c.mu.Lock()
			c.selectedWorkspace = ws.Path
			c.viewingLocalRun = false
			c.refreshWorkflowsLocked(ws.Path)
			c.mu.Unlock()
			_ = ctx.Host.Toast(ctx, "success", tr(loc, "toast.repo_added", ws.Name))
			c.notify(ctx)
			return nil
		},

		"select_workspace": func(ctx *extsdk.EventContext, path string) error {
			c.mu.Lock()
			c.selectedWorkspace = path
			c.viewingLocalRun = false
			c.refreshWorkflowsLocked(path)
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"remove_workspace": func(ctx *extsdk.EventContext, path string) error {
			if err := c.store.RemoveWorkspace(ctx, path); err != nil {
				return err
			}
			c.mu.Lock()
			delete(c.discovered, path)
			c.viewingLocalRun = false
			c.syncSelectionLocked()
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"select_workflow": func(ctx *extsdk.EventContext, file string) error {
			c.mu.Lock()
			c.selectedWorkflow = file
			c.viewingLocalRun = false
			c.refreshWorkflowsLocked(c.selectedWorkspace)
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"select_event": func(ctx *extsdk.EventContext, ev string) error {
			c.mu.Lock()
			c.selectedEvent = ev
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"select_job": func(ctx *extsdk.EventContext, job string) error {
			parsed := parseJobValue(job)
			c.mu.Lock()
			c.selectedJob = parsed
			if parsed != AllJobsOption {
				c.inspectedJobID = parsed
			}
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"set_preview_mode": func(ctx *extsdk.EventContext, val string) error {
			if mode, ok := parsePreviewModeValue(val); ok {
				c.mu.Lock()
				c.previewMode = mode
				c.mu.Unlock()
				c.notify(ctx)
			}
			return nil
		},

		"inspect_job": func(ctx *extsdk.EventContext, jobID string) error {
			id := strings.TrimSpace(jobID)
			if id == "" {
				return nil
			}
			c.mu.Lock()
			c.inspectedJobID = id
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"show_local_run": func(ctx *extsdk.EventContext) error {
			c.mu.Lock()
			c.viewingLocalRun = true
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"show_local_preview": func(ctx *extsdk.EventContext) error {
			c.mu.Lock()
			c.viewingLocalRun = false
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"run_job_direct": func(ctx *extsdk.EventContext, jobID string) error {
			return c.triggerLocalRunForJob(ctx, strings.TrimSpace(jobID), false)
		},

		"dryrun_job_direct": func(ctx *extsdk.EventContext, jobID string) error {
			return c.triggerLocalRunForJob(ctx, strings.TrimSpace(jobID), true)
		},

		"set_platform_image": func(ctx *extsdk.EventContext, val string) error {
			c.mu.Lock()
			c.platformImage = strings.TrimSpace(val)
			c.mu.Unlock()
			return nil
		},

		"toggle_bind_workdir": func(ctx *extsdk.EventContext, checked bool) error {
			c.mu.Lock()
			c.bindWorkdir = checked
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"set_dispatch_input": func(ctx *extsdk.EventContext) error {
			name := ctx.ArgString(0)
			if name == "" {
				return nil
			}
			val := ctx.EventValue()
			if len(ctx.Args) > 1 {
				if v := ctx.ArgString(1); v != "" || val == "" {
					val = v
				}
			}
			c.mu.Lock()
			c.dispatchInputs[name] = val
			c.mu.Unlock()
			return nil
		},

		"run_local": func(ctx *extsdk.EventContext) error {
			return c.triggerLocalRun(ctx, false)
		},

		"dryrun_local": func(ctx *extsdk.EventContext) error {
			return c.triggerLocalRun(ctx, true)
		},

		"cancel_local_run": func(ctx *extsdk.EventContext, runID string) error {
			if c.localMgr != nil {
				c.localMgr.CancelRun(runID)
			}
			c.notify(ctx)
			return nil
		},

		// --- Secrets dialog handlers ---
		"open_secrets": func(ctx *extsdk.EventContext) error {
			if keys, err := ctx.Host.SecretList(ctx); err == nil {
				c.store.SetSecretKeys(keys)
			}
			c.mu.Lock()
			c.openDialog = dialogSecrets
			c.dialogError = ""
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"save_secret": func(ctx *extsdk.EventContext) error {
			loc := c.resolveLocale(ctx.Locale)
			name := strings.TrimSpace(ctx.Field("secret_name"))
			val := ctx.Field("secret_value")
			if name == "" || val == "" {
				c.setDialogError(tr(loc, "dlg.secrets.err_required"))
				c.notify(ctx)
				return nil
			}
			if err := ctx.Host.SecretSet(ctx, name, val); err != nil {
				c.setDialogError(tr(loc, "dlg.secrets.err_save", err.Error()))
				c.notify(ctx)
				return nil
			}
			c.redactor.Add(name, val)
			if keys, err := ctx.Host.SecretList(ctx); err == nil {
				c.store.SetSecretKeys(keys)
			}
			c.setDialogError("")
			_ = ctx.Host.Toast(ctx, "success", tr(loc, "dlg.secrets.toast_saved", name))
			c.notify(ctx)
			return nil
		},

		"delete_secret": func(ctx *extsdk.EventContext, name string) error {
			loc := c.resolveLocale(ctx.Locale)
			if err := ctx.Host.SecretDelete(ctx, name); err != nil {
				c.setDialogError(tr(loc, "dlg.secrets.err_delete", err.Error()))
				c.notify(ctx)
				return nil
			}
			c.redactor.Remove(name)
			if keys, err := ctx.Host.SecretList(ctx); err == nil {
				c.store.SetSecretKeys(keys)
			}
			c.notify(ctx)
			return nil
		},

		// --- Tab 2 (Self-Hosted Runners) handlers ---
		"open_add_runner": func(ctx *extsdk.EventContext) error {
			c.mu.Lock()
			c.openDialog = dialogAddRunner
			c.dialogError = ""
			c.newRunnerScope = "repo"
			c.newRunnerAuthMode = "pat"
			c.newRunnerDocker = false
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"set_new_runner_scope": func(ctx *extsdk.EventContext, val string) error {
			c.mu.Lock()
			c.newRunnerScope = parseScopeValue(val)
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"set_new_runner_auth": func(ctx *extsdk.EventContext, val string) error {
			c.mu.Lock()
			c.newRunnerAuthMode = parseAuthModeValue(val)
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"toggle_new_runner_docker": func(ctx *extsdk.EventContext, checked bool) error {
			c.mu.Lock()
			c.newRunnerDocker = checked
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"create_runner": func(ctx *extsdk.EventContext) error {
			loc := c.resolveLocale(ctx.Locale)
			c.mu.Lock()
			scope := c.newRunnerScope
			authMode := c.newRunnerAuthMode
			allowDocker := c.newRunnerDocker
			c.mu.Unlock()

			target := strings.TrimSpace(ctx.Field("new_runner_target"))
			name := strings.TrimSpace(ctx.Field("new_runner_name"))
			tokenVal := strings.TrimSpace(ctx.Field("new_runner_token"))
			labelsRaw := strings.TrimSpace(ctx.Field("new_runner_labels"))
			image := strings.TrimSpace(ctx.Field("new_runner_image"))
			concRaw := strings.TrimSpace(ctx.Field("new_runner_concurrency"))

			if target == "" {
				c.setDialogError(tr(loc, "dlg.runner.err_target"))
				c.notify(ctx)
				return nil
			}
			if scope == "repo" && !strings.Contains(target, "/") {
				c.setDialogError(tr(loc, "dlg.runner.err_repo_format"))
				c.notify(ctx)
				return nil
			}
			if authMode == "jit" && tokenVal == "" {
				c.setDialogError(tr(loc, "dlg.runner.err_jit_token"))
				c.notify(ctx)
				return nil
			}
			if authMode == "pat" && tokenVal == "" && !c.store.Settings().HasDefaultPAT {
				c.setDialogError(tr(loc, "dlg.runner.err_pat_token"))
				c.notify(ctx)
				return nil
			}

			conc := 1
			if n, err := strconv.Atoi(concRaw); err == nil && n >= 1 && n <= 8 {
				conc = n
			}
			var labels []string
			for _, part := range strings.Split(labelsRaw, ",") {
				if p := strings.TrimSpace(part); p != "" {
					labels = append(labels, p)
				}
			}
			if len(labels) == 0 {
				labels = state.DefaultArchLabels(c.store.HostInfo().Arch)
			}

			runnerID := state.NewID("runr")
			secretKey := "setting:github_pat"
			if tokenVal != "" {
				secretKey = "runner_" + strings.ReplaceAll(runnerID, "-", "_") + "_cred"
				if err := ctx.Host.SecretSet(ctx, secretKey, tokenVal); err != nil {
					c.setDialogError(tr(loc, "dlg.runner.err_store_cred", err.Error()))
					c.notify(ctx)
					return nil
				}
				c.redactor.Add(secretKey, tokenVal)
				if keys, err := ctx.Host.SecretList(ctx); err == nil {
					c.store.SetSecretKeys(keys)
				}
			}

			cfg, err := c.store.SaveRunner(ctx, state.RunnerConfig{
				ID:             runnerID,
				Name:           name,
				Scope:          scope,
				Target:         target,
				AuthMode:       authMode,
				TokenSecretKey: secretKey,
				Labels:         labels,
				Image:          image,
				Concurrency:    conc,
				AllowDocker:    allowDocker,
				Enabled:        true,
			})
			if err != nil {
				c.setDialogError(err.Error())
				c.notify(ctx)
				return nil
			}

			c.mu.Lock()
			c.selectedRunnerID = cfg.ID
			c.openDialog = dialogNone
			c.dialogError = ""
			c.mu.Unlock()

			if c.runnerMgr != nil {
				c.runnerMgr.Reconcile()
			}
			_ = ctx.Host.Toast(ctx, "success", tr(loc, "dlg.runner.toast_started", cfg.Name))
			c.notify(ctx)
			return nil
		},

		"select_runner": func(ctx *extsdk.EventContext, id string) error {
			c.mu.Lock()
			c.selectedRunnerID = id
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"start_runner": func(ctx *extsdk.EventContext, id string) error {
			if c.runnerMgr != nil {
				if err := c.runnerMgr.StartRunner(ctx, id); err != nil {
					_ = ctx.Host.Toast(ctx, "error", err.Error())
					return err
				}
			}
			c.notify(ctx)
			return nil
		},

		"stop_runner": func(ctx *extsdk.EventContext, id string) error {
			if c.runnerMgr != nil {
				if err := c.runnerMgr.StopRunner(ctx, id); err != nil {
					_ = ctx.Host.Toast(ctx, "error", err.Error())
					return err
				}
			}
			c.notify(ctx)
			return nil
		},

		"delete_runner": func(ctx *extsdk.EventContext, id string) error {
			cfg, ok := c.store.RunnerByID(id)
			if c.runnerMgr != nil {
				_ = c.runnerMgr.StopRunner(ctx, id)
			}
			if ok && strings.HasPrefix(cfg.TokenSecretKey, "runner_") {
				_ = ctx.Host.SecretDelete(ctx, cfg.TokenSecretKey)
				c.redactor.Remove(cfg.TokenSecretKey)
			}
			if err := c.store.DeleteRunner(ctx, id); err != nil {
				return err
			}
			c.SyncSelectionDefaults()
			c.notify(ctx)
			return nil
		},

		// --- Tab 3 (History & Synergy) handlers ---
		"set_history_filter": func(ctx *extsdk.EventContext, val string) error {
			if f, ok := parseHistoryFilterValue(val); ok {
				c.mu.Lock()
				c.historyFilter = f
				c.mu.Unlock()
				c.notify(ctx)
			}
			return nil
		},

		"select_history_run": func(ctx *extsdk.EventContext, runID string) error {
			c.mu.Lock()
			c.selectedHistoryRun = runID
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"clear_history": func(ctx *extsdk.EventContext) error {
			if err := c.store.ClearFinishedHistory(ctx); err != nil {
				return err
			}
			c.mu.Lock()
			c.selectedHistoryRun = ""
			c.syncSelectionLocked()
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},

		"reproduce_locally": func(ctx *extsdk.EventContext, runID string) error {
			loc := c.resolveLocale(ctx.Locale)
			run, ok := c.store.RunByID(runID)
			if !ok {
				return fmt.Errorf("run %q not found", runID)
			}
			msg := c.loadRunIntoLocalTab(loc, run)
			_ = ctx.Host.Toast(ctx, "info", msg)
			c.notify(ctx)
			return nil
		},

		// --- Shared UI intents ---
		"open_shell": func(ctx *extsdk.EventContext) error {
			loc := c.resolveLocale(ctx.Locale)
			targetType := ctx.ArgString(0)
			targetID := ctx.ArgString(1)
			if targetType == "" || targetID == "" {
				return errors.New("missing open_shell target")
			}
			if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
				TargetType: targetType,
				TargetID:   targetID,
			}); err != nil {
				_ = ctx.Host.Toast(ctx, "error", tr(loc, "toast.open_shell_failed", err.Error()))
				return err
			}
			return nil
		},

		"copy_logs": func(ctx *extsdk.EventContext, runID string) error {
			loc := c.resolveLocale(ctx.Locale)
			run, ok := c.store.RunByID(runID)
			if !ok {
				return nil
			}
			text := formatLogsForCode(loc, run.Logs, c.redactor)
			if err := ctx.Host.Copy(ctx, text); err != nil {
				_ = ctx.Host.Toast(ctx, "error", tr(loc, "toast.copy_failed", err.Error()))
				return err
			}
			_ = ctx.Host.Toast(ctx, "success", tr(loc, "toast.logs_copied"))
			return nil
		},

		"close_dialog": func(ctx *extsdk.EventContext) error {
			c.mu.Lock()
			c.openDialog = dialogNone
			c.dialogError = ""
			c.mu.Unlock()
			c.notify(ctx)
			return nil
		},
	}
}

func (c *Controller) triggerLocalRun(ctx *extsdk.EventContext, dryRun bool) error {
	return c.triggerLocalRunForJob(ctx, "", dryRun)
}

func (c *Controller) triggerLocalRunForJob(ctx *extsdk.EventContext, overrideJobID string, dryRun bool) error {
	if c.localMgr == nil {
		return errors.New("local workflow manager unavailable")
	}

	c.mu.Lock()
	wsPath := c.selectedWorkspace
	wfFile := c.selectedWorkflow
	evName := c.selectedEvent
	jobID := c.selectedJob
	if overrideJobID != "" {
		jobID = overrideJobID
	}
	platImg := strings.TrimSpace(ctx.Field("platform_image"))
	if platImg == "" {
		platImg = c.platformImage
	}
	bindWD := c.bindWorkdir
	inputs := make(map[string]string, len(c.dispatchInputs))
	for k, v := range c.dispatchInputs {
		if fv := ctx.Field(sanitizeNodeKey("input_" + k)); fv != "" {
			inputs[k] = fv
		} else {
			inputs[k] = v
		}
	}
	c.mu.Unlock()

	if jobID == AllJobsOption {
		jobID = ""
	}

	secrets := c.loadProjectSecrets(ctx, ctx.Host)

	rec, err := c.localMgr.StartRun(ctx, localact.RunRequest{
		WorkspacePath: wsPath,
		WorkflowFile:  wfFile,
		Event:         evName,
		JobID:         jobID,
		PlatformImage: platImg,
		Inputs:        inputs,
		Secrets:       secrets,
		BindWorkdir:   bindWD,
		DryRun:        dryRun,
	})
	if err != nil {
		_ = ctx.Host.Toast(ctx, "error", err.Error())
		return err
	}

	c.mu.Lock()
	c.selectedLocalRun = rec.ID
	c.viewingLocalRun = true
	c.mu.Unlock()
	c.notify(ctx)
	return nil
}

func (c *Controller) loadProjectSecrets(ctx context.Context, host *extsdk.Host) map[string]string {
	out := map[string]string{}
	if host == nil {
		return out
	}
	keys, err := host.SecretList(ctx)
	if err != nil {
		return out
	}
	c.store.SetSecretKeys(keys)
	for _, k := range keys {
		// Skip internal runner credentials when injecting local workflow secrets.
		if strings.HasPrefix(k, "runner_") {
			continue
		}
		if v, ok, err := host.SecretGet(ctx, k); err == nil && ok && v != "" {
			out[k] = v
			c.redactor.Add(k, v)
		}
	}
	return out
}

func (c *Controller) loadRunIntoLocalTab(loc string, run *state.RunRecord) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.activeTab = TabLocal
	c.viewingLocalRun = false
	workspaces := c.store.Workspaces()

	// Match workspace: exact path for local runs, or repo name match for self-hosted runner runs.
	matchedWS := ""
	if run.WorkspacePath != "" {
		matchedWS = run.WorkspacePath
	} else if run.Target != "" {
		repoBase := run.Target
		if _, r, ok := strings.Cut(run.Target, "/"); ok && r != "" {
			repoBase = r
		}
		for _, ws := range workspaces {
			if strings.EqualFold(ws.Name, repoBase) {
				matchedWS = ws.Path
				break
			}
		}
	}
	if matchedWS != "" {
		c.selectedWorkspace = matchedWS
		c.refreshWorkflowsLocked(matchedWS)
	}
	if run.WorkflowFile != "" {
		c.selectedWorkflow = run.WorkflowFile
		c.refreshWorkflowsLocked(c.selectedWorkspace)
	}
	if run.Event != "" {
		c.selectedEvent = run.Event
	}
	if run.JobID != "" {
		// Check if any discovered workflow contains this job ID.
		if wfs := c.discovered[c.selectedWorkspace]; len(wfs) > 0 {
			for _, w := range wfs {
				for _, j := range w.Jobs {
					if strings.EqualFold(j.ID, run.JobID) || strings.EqualFold(j.Name, run.JobID) {
						c.selectedWorkflow = w.File
						c.selectedJob = j.ID
						c.inspectedJobID = j.ID
						return tr(loc, "toast.repro_loaded_job", w.File, j.ID)
					}
				}
			}
		}
		c.selectedJob = AllJobsOption
		if matchedWS == "" {
			return tr(loc, "toast.repro_add_repo", run.Target, run.JobID)
		}
	}
	return tr(loc, "toast.repro_loaded")
}

func (c *Controller) refreshHostState(ctx context.Context) {
	// Refresh from Host API when called from an EventContext.
	if evCtx, ok := ctx.(*extsdk.EventContext); ok && evCtx.Host != nil {
		if info, err := evCtx.Host.Info(ctx); err == nil {
			c.store.SetHostInfo(info)
		}
		if settings, err := evCtx.Host.Settings(ctx); err == nil {
			c.store.ApplySettings(settings.Raw())
		}
		if keys, err := evCtx.Host.SecretList(ctx); err == nil {
			c.store.SetSecretKeys(keys)
		}
	}
}

func (c *Controller) handleCommandRunLocal(ctx *extsdk.CommandContext) error {
	loc := c.resolveLocale(ctx.Locale)
	c.mu.Lock()
	c.activeTab = TabLocal
	hasSelection := c.selectedWorkspace != "" && c.selectedWorkflow != ""
	ws := c.selectedWorkspace
	wf := c.selectedWorkflow
	ev := c.selectedEvent
	job := c.selectedJob
	img := c.platformImage
	bindWD := c.bindWorkdir
	c.mu.Unlock()

	if !hasSelection {
		_ = ctx.Host.Toast(ctx, "info", tr(loc, "toast.cmd_select_first"))
		ctx.NotifyChanged()
		return nil
	}
	if job == AllJobsOption {
		job = ""
	}
	rec, err := c.localMgr.StartRun(ctx, localact.RunRequest{
		WorkspacePath: ws,
		WorkflowFile:  wf,
		Event:         ev,
		JobID:         job,
		PlatformImage: img,
		Secrets:       c.loadProjectSecrets(ctx, ctx.Host),
		BindWorkdir:   bindWD,
	})
	if err != nil {
		_ = ctx.Host.Toast(ctx, "error", err.Error())
		return err
	}
	c.mu.Lock()
	c.selectedLocalRun = rec.ID
	c.viewingLocalRun = true
	c.mu.Unlock()
	ctx.NotifyChanged()
	return nil
}

func (c *Controller) handleCommandAddRunner(ctx *extsdk.CommandContext) error {
	c.mu.Lock()
	c.activeTab = TabRunners
	c.openDialog = dialogAddRunner
	c.dialogError = ""
	c.mu.Unlock()
	ctx.NotifyChanged()
	return nil
}

func (c *Controller) handleToolListRunners(_ *extsdk.ToolContext) (any, error) {
	cfgs := c.store.Runners()
	slots := c.store.AllRunnerSlots()
	type item struct {
		ID          string                 `json:"id"`
		Name        string                 `json:"name"`
		Scope       string                 `json:"scope"`
		Target      string                 `json:"target"`
		Labels      []string               `json:"labels"`
		Concurrency int                    `json:"concurrency"`
		AllowDocker bool                   `json:"allow_docker"`
		Enabled     bool                   `json:"enabled"`
		Slots       []state.RunnerLiveSlot `json:"slots"`
	}
	out := make([]item, 0, len(cfgs))
	for _, cfg := range cfgs {
		out = append(out, item{
			ID:          cfg.ID,
			Name:        cfg.Name,
			Scope:       cfg.Scope,
			Target:      cfg.Target,
			Labels:      cfg.Labels,
			Concurrency: cfg.Concurrency,
			AllowDocker: cfg.AllowDocker,
			Enabled:     cfg.Enabled,
			Slots:       slots[cfg.ID],
		})
	}
	return map[string]any{"runners": out}, nil
}

func (c *Controller) handleToolRunLocalWorkflow(ctx *extsdk.ToolContext, raw json.RawMessage) (any, error) {
	var args struct {
		RepoPath string `json:"repo_path"`
		Workflow string `json:"workflow"`
		Job      string `json:"job"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(args.RepoPath) == "" {
		return nil, errors.New("repo_path is required")
	}
	wfFile := strings.TrimSpace(args.Workflow)
	if wfFile == "" {
		wfs, err := workflows.Discover(args.RepoPath)
		if err != nil {
			return nil, err
		}
		if len(wfs) == 0 {
			return nil, fmt.Errorf("no workflows found in %s", args.RepoPath)
		}
		wfFile = wfs[0].File
	}
	rec, err := c.localMgr.StartRun(ctx, localact.RunRequest{
		WorkspacePath: args.RepoPath,
		WorkflowFile:  wfFile,
		JobID:         strings.TrimSpace(args.Job),
		Secrets:       c.loadProjectSecrets(ctx, ctx.Host),
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.selectedLocalRun = rec.ID
	c.viewingLocalRun = true
	c.mu.Unlock()
	if c.notifyFn != nil {
		c.notifyFn()
	}
	return map[string]any{
		"run_id":        rec.ID,
		"status":        rec.Status,
		"workflow_file": rec.WorkflowFile,
		"job_id":        rec.JobID,
	}, nil
}

func (c *Controller) setDialogError(msg string) {
	c.mu.Lock()
	c.dialogError = msg
	c.mu.Unlock()
}

func (c *Controller) notify(ctx *extsdk.EventContext) {
	if ctx != nil {
		ctx.NotifyChanged()
	} else if c.notifyFn != nil {
		c.notifyFn()
	}
}

// --- Helpers ---

func buildDagRows(loc string, steps []state.StepRecord) []extsdk.DagRow {
	if len(steps) == 0 {
		return nil
	}
	rows := make([]extsdk.DagRow, 0, len(steps))
	seen := make(map[string]bool, len(steps))
	for _, st := range steps {
		id := mask.TruncateRunes(mask.SanitizeInline(st.ID), 60)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var parents []string
		for _, p := range st.Parents {
			if len(parents) >= 8 {
				break
			}
			cp := mask.TruncateRunes(mask.SanitizeInline(p), 60)
			if cp != "" {
				parents = append(parents, cp)
			}
		}
		meta := formatDuration(st.StartedAt, st.EndedAt)
		if st.StartedAt.IsZero() {
			meta = statusLabel(loc, st.Status)
		}
		rows = append(rows, extsdk.DagRow{
			ID:      id,
			Parents: parents,
			Text:    mask.TruncateRunes(mask.SanitizeInline(st.Name), 120),
			Detail:  mask.TruncateRunes(mask.SanitizeInline(st.Detail), 120),
			Meta:    mask.TruncateRunes(mask.SanitizeInline(meta), 64),
			Tone:    statusTone(st.Status),
			Icon:    statusIcon(st.Status),
			Strong:  strings.HasPrefix(id, "job:"),
			Badges: []extsdk.Badge{
				{Text: mask.TruncateRunes(statusLabel(loc, st.Status), 20), Tone: statusTone(st.Status), Variant: "soft"},
			},
		})
	}
	return rows
}

func formatLogsForCode(loc string, lines []string, red *mask.Redactor) string {
	if len(lines) == 0 {
		return tr(loc, "local.waiting_output")
	}
	var b strings.Builder
	// Keep the newest lines fitting inside maxCodeLogBytes.
	start := len(lines)
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		add := len(lines[i]) + 1
		if total+add > maxCodeLogBytes && i < len(lines)-1 {
			break
		}
		total += add
		start = i
	}
	for i := start; i < len(lines); i++ {
		if i > start {
			b.WriteByte('\n')
		}
		b.WriteString(lines[i])
	}
	return red.SanitizeMultiline(b.String())
}

func runTitle(r *state.RunRecord) string {
	if r.Kind == "runner" {
		if r.JobID != "" {
			return fmt.Sprintf("%s · %s", r.Target, r.JobID)
		}
		return r.RunnerName
	}
	name := r.WorkflowName
	if name == "" {
		name = r.WorkflowFile
	}
	if r.JobID != "" {
		return fmt.Sprintf("%s (%s)", name, r.JobID)
	}
	return name
}

func statusTone(st string) string {
	switch strings.ToLower(strings.TrimSpace(st)) {
	case "success", "succeeded", "standby":
		return "success"
	case "failure", "failed", "error":
		return "danger"
	case "running", "running_job":
		return "accent"
	case "cancelled", "canceled", "skipped", "starting", "cleaning":
		return "warning"
	default:
		return "muted"
	}
}

func statusIcon(st string) string {
	switch strings.ToLower(strings.TrimSpace(st)) {
	case "success", "succeeded":
		return "circle-check"
	case "failure", "failed", "error":
		return "circle-x"
	case "running", "running_job":
		return "activity"
	case "cancelled", "canceled", "skipped":
		return "ban"
	default:
		return "clock"
	}
}

func formatDuration(start, end time.Time) string {
	if start.IsZero() {
		return "-"
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	d := end.Sub(start)
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Second).String()
}

func sanitizeNodeKey(s string) string {
	s = mask.SanitizeInline(s)
	s = strings.NewReplacer("/", "_", "#", "_", " ", "_").Replace(s)
	return mask.TruncateRunes(s, 100)
}

type workflowStage struct {
	Level int
	Jobs  []workflows.JobSummary
	Deps  []string
}

func computeWorkflowStages(jobs []workflows.JobSummary) []workflowStage {
	if len(jobs) == 0 {
		return nil
	}
	byID := make(map[string]workflows.JobSummary, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	memo := make(map[string]int, len(jobs))
	visiting := make(map[string]bool, len(jobs))
	var depthOf func(id string) int
	depthOf = func(id string) int {
		if lvl, ok := memo[id]; ok {
			return lvl
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		j := byID[id]
		best := 0
		for _, dep := range j.Needs {
			if _, ok := byID[dep]; !ok || dep == id {
				continue
			}
			if d := depthOf(dep) + 1; d > best {
				best = d
			}
		}
		delete(visiting, id)
		memo[id] = best
		return best
	}

	maxLvl := 0
	for _, j := range jobs {
		if lvl := depthOf(j.ID); lvl > maxLvl {
			maxLvl = lvl
		}
	}

	grouped := make([][]workflows.JobSummary, maxLvl+1)
	for _, j := range jobs {
		lvl := memo[j.ID]
		grouped[lvl] = append(grouped[lvl], j)
	}

	out := make([]workflowStage, 0, maxLvl+1)
	for lvl, list := range grouped {
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, k int) bool {
			return strings.ToLower(list[i].Name) < strings.ToLower(list[k].Name)
		})
		depSet := map[string]bool{}
		var deps []string
		for _, j := range list {
			for _, d := range j.Needs {
				if _, ok := byID[d]; ok && !depSet[d] {
					depSet[d] = true
					deps = append(deps, d)
				}
			}
		}
		sort.Strings(deps)
		out = append(out, workflowStage{
			Level: lvl,
			Jobs:  list,
			Deps:  deps,
		})
	}
	return out
}

type jobRunnerMeta struct {
	icon     string
	tone     string
	badge    string
	nonLinux bool
}

func classifyJobRunner(loc string, runsOn []string) jobRunnerMeta {
	if len(runsOn) == 0 {
		return jobRunnerMeta{icon: "terminal", tone: "success"}
	}
	joined := strings.ToLower(strings.Join(runsOn, " "))
	switch {
	case strings.Contains(joined, "${{"):
		return jobRunnerMeta{icon: "layers", tone: "muted", badge: tr(loc, "local.badge_matrix")}
	case strings.Contains(joined, "macos"):
		return jobRunnerMeta{icon: "laptop", tone: "warning", badge: "macOS", nonLinux: true}
	case strings.Contains(joined, "windows"):
		return jobRunnerMeta{icon: "monitor", tone: "warning", badge: "Windows", nonLinux: true}
	default:
		return jobRunnerMeta{icon: "terminal", tone: "success"}
	}
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	n := 0
	for i, r := range s {
		rl := utf8.RuneLen(r)
		if i+rl > maxBytes {
			break
		}
		n = i + rl
	}
	return s[:n]
}
