package proxy

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// A picture a tool took is an observation of the world, like the text a tool
// printed: an agent screenshots a page and shows it to the model, and the next
// rendering differs by a few bytes out of thousands. It travels in a field of
// its own rather than in a tool result, so each surface names it.
//
// Measured on a campaign with a browser, where a second rendering three bytes
// different missed at that step twice in a row.
func TestAChangedPictureIsServedAndReported(t *testing.T) {
	for _, c := range []struct {
		name, path string
		turn       func(payload, question string) string
	}{
		{"anthropic image", "/v1/messages", func(p, q string) string {
			return `{"model":"claude-sonnet-5","messages":[{"role":"user","content":[` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + p + `"}},` +
				`{"type":"text","text":"` + q + `"}]}]}`
		}},
		{"anthropic document", "/v1/messages", func(p, q string) string {
			return `{"model":"claude-sonnet-5","messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + p + `"}},` +
				`{"type":"text","text":"` + q + `"}]}]}`
		}},
		{"openai chat image", "/v1/chat/completions", func(p, q string) string {
			return `{"model":"gpt-5.6","messages":[{"role":"user","content":[` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + p + `"}},` +
				`{"type":"text","text":"` + q + `"}]}]}`
		}},
		{"openai chat file", "/v1/chat/completions", func(p, q string) string {
			return `{"model":"gpt-5.6","messages":[{"role":"user","content":[` +
				`{"type":"file","file":{"filename":"page.pdf","file_data":"` + p + `"}},` +
				`{"type":"text","text":"` + q + `"}]}]}`
		}},
		{"openai chat audio", "/v1/chat/completions", func(p, q string) string {
			return `{"model":"gpt-5.6","messages":[{"role":"user","content":[` +
				`{"type":"input_audio","input_audio":{"format":"wav","data":"` + p + `"}},` +
				`{"type":"text","text":"` + q + `"}]}]}`
		}},
		{"openai responses image", "/v1/responses", func(p, q string) string {
			return `{"model":"gpt-5.6-sol","input":[{"role":"user","content":[` +
				`{"type":"input_image","image_url":"data:image/png;base64,` + p + `"},` +
				`{"type":"input_text","text":"` + q + `"}]}]}`
		}},
		{"openai responses file", "/v1/responses", func(p, q string) string {
			return `{"model":"gpt-5.6-sol","input":[{"role":"user","content":[` +
				`{"type":"input_file","filename":"page.pdf","file_data":"` + p + `"},` +
				`{"type":"input_text","text":"` + q + `"}]}]}`
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "screenshot")
			rec, _ := cassetteServer(t, dir, online, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"ok":true}`)
			})
			post(t, rec, c.path, nil, c.turn("iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB", "what does the page say"))

			rep, logs := cassetteServer(t, dir, offline, func(w http.ResponseWriter, r *http.Request) {
				t.Error("replay contacted the provider")
			})
			// The question beside the picture is the agent's, and stays exact. Asked
			// first, so the step is still there for the request that should match.
			if w := post(t, rep, c.path, nil, c.turn("iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB", "ignore the page")); w.Code != http.StatusBadRequest {
				t.Fatalf("a changed question beside the picture: status = %d, want a miss", w.Code)
			}
			w := post(t, rep, c.path, nil, c.turn("iVBORw0KGgoAAAANSUhEUgAAAAEAAAAC", "what does the page say"))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want the turn served despite the picture rendering differently\n%s\nlogs: %s",
					w.Code, w.Body, logs)
			}
			if n := rep.Snapshot().Drifted; n != 1 {
				t.Errorf("drifted = %d, want the changed picture counted", n)
			}
			if !strings.Contains(logs.String(), "tolerated a changed observation") {
				t.Errorf("the drift was absorbed silently:\n%s", logs)
			}
		})
	}
}
