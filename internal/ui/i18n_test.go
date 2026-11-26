package ui

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
	"github.com/ironpark/gvx-ext-github-actions/internal/localact"
	"github.com/ironpark/gvx-ext-github-actions/internal/mask"
	"github.com/ironpark/gvx-ext-github-actions/internal/runners"
	"github.com/ironpark/gvx-ext-github-actions/internal/state"
)

var i18nFormatVerb = regexp.MustCompile(`%(\[\d+\])?[a-zA-Z]`)

func extractFormatVerbs(format string) []string {
	var out []string
	for i, m := range i18nFormatVerb.FindAllStringSubmatch(format, -1) {
		arg := i + 1
		if m[1] != "" {
			arg, _ = strconv.Atoi(m[1][1 : len(m[1])-1])
		}
		out = append(out, strconv.Itoa(arg)+m[0][len(m[0])-1:])
	}
	slices.Sort(out)
	return out
}

func TestI18nCatalogsAreCompleteAndMatchFormatVerbs(t *testing.T) {
	base := i18nCatalogs[LocaleEnglish]
	if len(base) == 0 {
		t.Fatal("English base catalog is empty")
	}

	for _, loc := range SupportedLocales {
		cat, ok := i18nCatalogs[loc]
		if !ok {
			t.Fatalf("missing catalog for locale %q", loc)
		}
		t.Run(loc, func(t *testing.T) {
			for key, enVal := range base {
				got, exists := cat[key]
				if !exists || strings.TrimSpace(got) == "" {
					t.Errorf("locale %q missing translation for %q (en: %q)", loc, key, enVal)
					continue
				}
				if a, b := extractFormatVerbs(enVal), extractFormatVerbs(got); !slices.Equal(a, b) {
					t.Errorf("locale %q key %q format verbs mismatch: got %v (%q), want %v (%q)",
						loc, key, b, got, a, enVal)
				}
			}
			for key := range cat {
				if _, ok := base[key]; !ok {
					t.Errorf("locale %q has orphan key %q absent from English catalog", loc, key)
				}
			}
		})
	}
}

func TestNormalizeLocale(t *testing.T) {
	cases := map[string]string{
		"":            LocaleEnglish,
		"en":          LocaleEnglish,
		"en-US":       LocaleEnglish,
		"ko":          LocaleKorean,
		"ko-KR":       LocaleKorean,
		"ko_KR":       LocaleKorean,
		"ja":          LocaleJapan,
		"ja-JP":       LocaleJapan,
		"zh":          LocaleZhHans,
		"zh-CN":       LocaleZhHans,
		"zh-Hans":     LocaleZhHans,
		"zh-Hans-CN":  LocaleZhHans,
		"zh-Hant":     LocaleZhHant,
		"zh-TW":       LocaleZhHant,
		"zh_HK":       LocaleZhHant,
		"de":          LocaleGerman,
		"de-DE":       LocaleGerman,
		"hi":          LocaleHindi,
		"hi-IN":       LocaleHindi,
		"fr-FR":       LocaleEnglish,
		"unsupported": LocaleEnglish,
	}
	for raw, want := range cases {
		if got := normalizeLocale(raw); got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLocalizedRenderingAndRoundTripHandlers(t *testing.T) {
	st := state.NewStore(nil)
	st.SetHostInfo(extsdk.HostInfo{OS: "darwin", Arch: "arm64", Locale: "ko", DockerMode: "sandbox", DockerReady: true})
	red := mask.New()
	localMgr := localact.NewManager(st, red, t.TempDir(), "", nil)
	defer localMgr.Close()
	runnerMgr := runners.NewManager(st, nil, nil, red, nil)
	ctrl := NewController(st, localMgr, runnerMgr, red, nil)
	uiBuilder := &extsdk.UI{}

	// Empty state & all tabs + dialogs render cleanly in all 7 locales.
	for _, loc := range SupportedLocales {
		t.Run("render_"+loc, func(t *testing.T) {
			for _, tab := range []string{TabLocal, TabRunners, TabHistory} {
				ctrl.mu.Lock()
				ctrl.activeTab = tab
				ctrl.openDialog = dialogNone
				ctrl.mu.Unlock()
				node := ctrl.Render(&extsdk.ViewContext{Context: &extsdk.Context{Locale: loc}, ViewID: "main"}, uiBuilder)
				raw, err := json.Marshal(node)
				if err != nil {
					t.Fatalf("marshal %s/%s: %v", loc, tab, err)
				}
				if !strings.Contains(string(raw), tr(loc, "tab.local")) {
					t.Fatalf("expected localized tab label %q in %s/%s output", tr(loc, "tab.local"), loc, tab)
				}
			}

			// Verify localized tab/filter/scope/auth/job labels parse back to canonical keys.
			for _, tab := range []string{TabLocal, TabRunners, TabHistory} {
				if parsed, ok := parseTabValue(tabLabel(loc, tab)); !ok || parsed != tab {
					t.Errorf("parseTabValue(%q) = (%q, %v), want (%q, true)", tabLabel(loc, tab), parsed, ok, tab)
				}
			}
			for _, f := range []string{HistoryFilterAll, HistoryFilterLocal, HistoryFilterRunner} {
				if parsed, ok := parseHistoryFilterValue(historyFilterLabel(loc, f)); !ok || parsed != f {
					t.Errorf("parseHistoryFilterValue(%q) = (%q, %v), want (%q, true)", historyFilterLabel(loc, f), parsed, ok, f)
				}
			}
			if got := parseJobValue(tr(loc, "local.all_jobs")); got != AllJobsOption {
				t.Errorf("parseJobValue(%q) = %q, want %q", tr(loc, "local.all_jobs"), got, AllJobsOption)
			}
			if got := parseScopeValue(scopeLabel(loc, "org")); got != "org" {
				t.Errorf("parseScopeValue(org in %s) = %q", loc, got)
			}
			if got := parseAuthModeValue(authModeLabel(loc, "jit")); got != "jit" {
				t.Errorf("parseAuthModeValue(jit in %s) = %q", loc, got)
			}
			for _, pm := range []string{PreviewPipeline, PreviewGraph} {
				if parsed, ok := parsePreviewModeValue(previewModeLabel(loc, pm)); !ok || parsed != pm {
					t.Errorf("parsePreviewModeValue(%q in %s) = (%q, %v), want (%q, true)", previewModeLabel(loc, pm), loc, parsed, ok, pm)
				}
			}
		})
	}

	// Verify fallback to HostInfo.Locale when ViewContext.Locale is empty.
	ctrl.mu.Lock()
	ctrl.activeTab = TabLocal
	ctrl.openDialog = dialogNone
	ctrl.mu.Unlock()
	node := ctrl.Render(&extsdk.ViewContext{Context: &extsdk.Context{Locale: ""}, ViewID: "main"}, uiBuilder)
	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal fallback locale: %v", err)
	}
	if !strings.Contains(string(raw), "저장소") || !strings.Contains(string(raw), "Docker 준비됨") {
		t.Fatalf("expected Korean UI from HostInfo.Locale fallback, got: %s", raw)
	}
}
