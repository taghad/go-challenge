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

func TestIndexStatesThatTheNumberIsApproximate(t *testing.T) {
	page := string(Index)
	if !strings.Contains(page, "approximate") {
		t.Error("page does not state that the number is approximate")
	}
	if !strings.Contains(page, "as_of") {
		t.Error("page does not show as_of")
	}
}

// The segment goes into the URL path, so it must be encoded.
func TestIndexEncodesSegmentInURL(t *testing.T) {
	if !regexp.MustCompile(`encodeURIComponent\(segment\)`).Match(Index) {
		t.Error("page does not encode the segment")
	}
}
