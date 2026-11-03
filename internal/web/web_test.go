package web

import (
	"regexp"
	"strings"
	"testing"
)

// All output must use textContent, so that nothing a user typed or the server returned
// can become markup. The page is simple enough for a text check to catch a regression.
func TestIndexRendersOnlyWithTextContent(t *testing.T) {
	page := string(Index)
	for _, unsafe := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(page, unsafe) {
			t.Errorf("page uses %s", unsafe)
		}
	}
	if !strings.Contains(page, ".textContent = ") {
		t.Error("page does not use textContent")
	}
}

func TestIndexHasTwoForms(t *testing.T) {
	page := string(Index)
	for _, id := range []string{`id="event-form"`, `id="estimate-form"`} {
		if !strings.Contains(page, id) {
			t.Errorf("page has no %s", id)
		}
	}
}

func TestIndexPutsTheEstimateFirst(t *testing.T) {
	page := string(Index)
	estimate := strings.Index(page, `id="estimate-form"`)
	event := strings.Index(page, `id="event-form"`)
	if estimate < 0 || event < 0 || estimate > event {
		t.Errorf("estimate form at %d, event form at %d, want the estimate first", estimate, event)
	}
	if !strings.Contains(page, "Send a test event") {
		t.Error("page does not name the event form")
	}
}

// One embedded file: nothing may load from another origin.
func TestIndexLoadsNothingExternal(t *testing.T) {
	page := string(Index)
	for _, external := range []string{"http://", "https://", "<link", "<script src", "@import", "url("} {
		if strings.Contains(page, external) {
			t.Errorf("page contains %s", external)
		}
	}
}

func TestIndexSupportsBothThemesAndMobileWidth(t *testing.T) {
	page := string(Index)
	for _, want := range []string{"color-scheme: light dark", "prefers-color-scheme: dark", `name="viewport"`, "max-width: 30rem"} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestIndexStatesThatTheNumberIsApproximate(t *testing.T) {
	page := string(Index)
	if !strings.Contains(page, "approximate") {
		t.Error("page does not state that the number is approximate")
	}
	if !strings.Contains(page, "as_of") {
		t.Error("page does not show as_of")
	}
}

func TestIndexLinksToTheReport(t *testing.T) {
	if !strings.Contains(string(Index), `href="/report"`) {
		t.Error("page does not link to /report")
	}
}

func TestReportStatesItsLimitsAndLinksBack(t *testing.T) {
	page := string(Report)
	for _, want := range []string{"Not production capacity", `href="/"`, "<svg"} {
		if !strings.Contains(page, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
}

// The segment goes into the URL path, so it must be encoded.
func TestIndexEncodesSegmentInURL(t *testing.T) {
	if !regexp.MustCompile(`encodeURIComponent\(segment\)`).Match(Index) {
		t.Error("page does not encode the segment")
	}
}
