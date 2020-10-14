// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logtree

import (
	"fmt"
	"testing"
)

func TestJournalRetention(t *testing.T) {
	j := newJournal()

	for i := 0; i < 9000; i += 1 {
		e := &entry{
			origin:  "main",
			leveled: testPayload(fmt.Sprintf("test %d", i)),
		}
		j.append(e)
	}

	entries := j.getEntries("main")
	if want, got := 8192, len(entries); want != got {
		t.Fatalf("wanted %d entries, got %d", want, got)
	}
	for i, entry := range entries {
		want := fmt.Sprintf("test %d", (9000-8192)+i)
		got := entry.leveled.message
		if want != got {
			t.Fatalf("wanted entry %q, got %q", want, got)
		}
	}
}

func TestJournalQuota(t *testing.T) {
	j := newJournal()

	for i := 0; i < 9000; i += 1 {
		j.append(&entry{
			origin:  "chatty",
			leveled: testPayload(fmt.Sprintf("chatty %d", i)),
		})
		if i%10 == 0 {
			j.append(&entry{
				origin:  "solemn",
				leveled: testPayload(fmt.Sprintf("solemn %d", i)),
			})
		}
	}

	entries := j.getEntries("chatty")
	if want, got := 8192, len(entries); want != got {
		t.Fatalf("wanted %d chatty entries, got %d", want, got)
	}
	entries = j.getEntries("solemn")
	if want, got := 900, len(entries); want != got {
		t.Fatalf("wanted %d solemn entries, got %d", want, got)
	}
	entries = j.getEntries("absent")
	if want, got := 0, len(entries); want != got {
		t.Fatalf("wanted %d absent entries, got %d", want, got)
	}

	entries = j.scanEntries(filterAll())
	if want, got := 8192+900, len(entries); want != got {
		t.Fatalf("wanted %d total entries, got %d", want, got)
	}
	setMessages := make(map[string]bool)
	for _, entry := range entries {
		setMessages[entry.leveled.message] = true
	}

	for i := 0; i < 900; i += 1 {
		want := fmt.Sprintf("solemn %d", i*10)
		if !setMessages[want] {
			t.Fatalf("could not find entry %q in journal", want)
		}
	}
	for i := 0; i < 8192; i += 1 {
		want := fmt.Sprintf("chatty %d", i+(9000-8192))
		if !setMessages[want] {
			t.Fatalf("could not find entry %q in journal", want)
		}
	}
}

func TestJournalSubtree(t *testing.T) {
	j := newJournal()
	j.append(&entry{origin: "a", leveled: testPayload("a")})
	j.append(&entry{origin: "a.b", leveled: testPayload("a.b")})
	j.append(&entry{origin: "a.b.c", leveled: testPayload("a.b.c")})
	j.append(&entry{origin: "a.b.d", leveled: testPayload("a.b.d")})
	j.append(&entry{origin: "e.f", leveled: testPayload("e.f")})
	j.append(&entry{origin: "e.g", leveled: testPayload("e.g")})

	expect := func(f filter, msgs ...string) string {
		res := j.scanEntries(f)
		set := make(map[string]bool)
		for _, entry := range res {
			set[entry.leveled.message] = true
		}

		for _, want := range msgs {
			if !set[want] {
				return fmt.Sprintf("missing entry %q", want)
			}
		}
		return ""
	}

	if res := expect(filterAll(), "a", "a.b", "a.b.c", "a.b.d", "e.f", "e.g"); res != "" {
		t.Fatalf("All: %s", res)
	}
	if res := expect(filterSubtree("a"), "a", "a.b", "a.b.c", "a.b.d"); res != "" {
		t.Fatalf("Subtree(a): %s", res)
	}
	if res := expect(filterSubtree("a.b"), "a.b", "a.b.c", "a.b.d"); res != "" {
		t.Fatalf("Subtree(a.b): %s", res)
	}
	if res := expect(filterSubtree("e"), "e.f", "e.g"); res != "" {
		t.Fatalf("Subtree(a.b): %s", res)
	}
}
