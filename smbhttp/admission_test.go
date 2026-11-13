package smbhttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"testing"

	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

type overloadError struct{ overloaded bool }

func (overloadError) Error() string      { return "secret backend diagnostic" }
func (e overloadError) Overloaded() bool { return e.overloaded }

type busyOpener struct{ readerFunc }

func (b busyOpener) Open(ctx context.Context, s, f string) (fs.File, error) {
	_, err := b.ReadFile(ctx, s, f)
	return nil, err
}

func TestOverloadMarkerAcrossHTTPModes(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, marked := range []bool{false, true} {
			reader := readerFunc(func(context.Context, string, string) ([]byte, error) {
				return nil, fmt.Errorf("private wrapper: %w", overloadError{marked})
			})
			var source smbhttp.FileReader = reader
			if streaming {
				source = busyOpener{reader}
			}
			h := handler(t, source, smbhttp.Options{})
			status, message := 502, "SMB request failed"
			if marked {
				status, message = 503, "SMB reader busy"
			}
			for _, method := range []string{"GET", "HEAD"} {
				w := call(h, method, "/share/file", "")
				if w.Code != status {
					t.Fatalf("streaming=%v marked=%v %s: %v", streaming, marked, method, w)
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Fatal("HEAD wrote body")
				}
				if method == "GET" {
					var body map[string]string
					if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["error"] != message {
						t.Fatal(w.Body.String())
					}
				}
			}
			w := call(h, "POST", "/share/batch", `{"files":[{"fileName":"file"}]}`)
			var body smbhttp.BatchFileResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Files) != 1 || body.Files[0].Error != message {
				t.Fatal(w.Body.String())
			}
		}
	}
}
