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

package oauthpending

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sealed payload opens only under its own handle, kind and row.
func TestSealedRecordOpensOnlyWhereItWasSealed(t *testing.T) {
	h := strings.Repeat("a", MinHandleLen)
	other := strings.Repeat("b", MinHandleLen)
	id, sealed, err := seal(KindFlow, h, []byte("payload"))
	require.NoError(t, err)

	got, err := open(KindFlow, h, id, sealed)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(got))

	_, err = open(KindFlow, other, id, sealed)
	assert.Error(t, err, "another handle")
	_, err = open(KindTicket, h, id, sealed)
	assert.Error(t, err, "another kind")
	_, err = open(KindFlow, h, recordID(KindFlow, other), sealed)
	assert.Error(t, err, "moved to another row")
	assert.NotEqual(t, recordID(KindFlow, h), recordID(KindTicket, h))
}
