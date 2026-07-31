/*
Copyright 2026 The KubeVela Authors.

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

package definition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// propertiesHashLen is long enough that two property sets colliding is not a
// practical concern, and short enough to leave the readable part of a key visible.
const propertiesHashLen = 8

// cacheIdentity assembles the name of the backing Config object from a source's
// resolved storage.key and its properties.
//
// storage.key is generated from the context the template reads, and covers only
// that. Properties are appended as a hash here rather than being written into the
// key, for two reasons: a property may contain characters that are illegal in an
// object name - an image reference or an entity ref, say - and hashing removes any
// chance of a definition failing to discriminate on an input that changes its
// output.
//
// The readable portion leads, so `vela config list | grep <definition>` still
// finds what an operator is looking for.
func cacheIdentity(key string, props map[string]interface{}) (string, error) {
	identity := key
	if len(props) > 0 {
		sum, err := hashProperties(props)
		if err != nil {
			return "", err
		}
		identity = key + "-" + sum
	}

	// The generated key is static text, so how long it resolves to is only known
	// now. Reduce rather than reject: an over-long key is an accident of the
	// values, not an authoring error the user can act on. Hash the whole thing
	// rather than truncating, because truncation lets distinct keys collide.
	if len(identity) > MaxCacheKeyLen {
		sum := sha256.Sum256([]byte(identity))
		full := hex.EncodeToString(sum[:])
		// Keep a readable prefix where there is room for one.
		prefixLen := MaxCacheKeyLen - len(full) - 1
		if prefixLen > 0 && prefixLen <= len(identity) {
			return identity[:prefixLen] + "-" + full, nil
		}
		return full, nil
	}
	return identity, nil
}

// hashProperties fingerprints resolved source properties.
//
// json.Marshal sorts map keys, so the result does not depend on Go's map
// iteration order - without that, the same binding would address a different
// entry on each reconcile.
func hashProperties(props map[string]interface{}) (string, error) {
	raw, err := json.Marshal(props)
	if err != nil {
		return "", fmt.Errorf("hashing source properties: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:propertiesHashLen], nil
}
