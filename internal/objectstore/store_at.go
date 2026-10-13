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

package objectstore

import "fmt"

// NewStoreAt builds the durable ObjectStore for an address and credential the caller already holds,
// rather than reading them from OBJECT_STORE_* env as NewMinioStore does. The controller uses it to
// read uploaded skill bodies from the bundled store, whose address and credential it derives for the
// agent pods it renders. It reads and writes the same bucket as NewMinioStore.
func NewStoreAt(addr, accessKey, secretKey string) (ObjectStore, error) {
	client, err := NewMinioClient(addr, accessKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("durable object store: %w", err)
	}
	return &minioKBStore{client: client, bucket: knowledgeBaseBucket}, nil
}
