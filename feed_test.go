//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package main

import (
	"fmt"
	"testing"
)

type ErrorOnlyFeed struct {
	name string
}

func (t *ErrorOnlyFeed) Name() string {
	return t.name
}

func (t *ErrorOnlyFeed) Start() error {
	return fmt.Errorf("ErrorOnlyFeed Start() invoked")
}

func (t *ErrorOnlyFeed) Close() error {
	return fmt.Errorf("ErrorOnlyFeed Close() invoked")
}

func (t *ErrorOnlyFeed) Streams() map[string]Stream {
	return nil
}

func TestParsePartitionsToVBucketIds(t *testing.T) {
	v, err := ParsePartitionsToVBucketIds(nil)
	if err != nil || v == nil || len(v) != 0 {
		t.Errorf("expected empty")
	}
	v, err = ParsePartitionsToVBucketIds(map[string]Stream{})
	if err != nil || v == nil || len(v) != 0 {
		t.Errorf("expected empty")
	}
	v, err = ParsePartitionsToVBucketIds(map[string]Stream{"123": nil})
	if err != nil || v == nil || len(v) != 1 {
		t.Errorf("expected one entry")
	}
	if v[0] != uint16(123) {
		t.Errorf("expected 123")
	}
	v, err = ParsePartitionsToVBucketIds(map[string]Stream{"!bad": nil})
	if err == nil || v != nil {
		t.Errorf("expected error")
	}
}
