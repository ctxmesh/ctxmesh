/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ocr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPOCR_PostsRawBodyAndReturnsText(t *testing.T) {
	var gotBody []byte
	var gotCT string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ocr", r.URL.Path)
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"text":"hello ocr world","pages":2}`))
	}))
	defer ts.Close()

	o := NewHTTPOCR(ts.URL, nil)
	text, err := o.OCRPDF(context.Background(), []byte("%PDF-fake-bytes"))
	require.NoError(t, err)
	require.Equal(t, "hello ocr world", text)
	require.Equal(t, "application/pdf", gotCT)
	require.Equal(t, "%PDF-fake-bytes", string(gotBody), "the raw PDF bytes are posted verbatim (no multipart/base64)")
}

func TestHTTPOCR_ErrorsOnNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer ts.Close()

	o := NewHTTPOCR(ts.URL, nil)
	_, err := o.OCRPDF(context.Background(), []byte("x"))
	require.Error(t, err, "a non-200 is an error so the executor fails open to PartiallyIngested")
}
