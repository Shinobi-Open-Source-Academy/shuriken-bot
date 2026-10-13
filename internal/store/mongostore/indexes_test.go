package mongostore

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// migration002 is the single source of truth these specs must agree with.
const migration002 = "../../../migrations/002_shuriken_indexes.js"

var (
	reCollection = regexp.MustCompile(`collection:\s*'([^']+)'`)
	reIndexName  = regexp.MustCompile(`name:\s*'([^']+)'`)
)

// parseMigration002 returns collection -> index names, as declared in the JS.
func parseMigration002(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(migration002)
	if err != nil {
		t.Fatalf("read %s: %v", migration002, err)
	}

	// Each `collection:` key starts a block that runs to the next one.
	locs := reCollection.FindAllSubmatchIndex(src, -1)
	if len(locs) == 0 {
		t.Fatalf("no collections found in %s — did its shape change?", migration002)
	}

	out := make(map[string][]string, len(locs))
	for i, loc := range locs {
		name := string(src[loc[2]:loc[3]])
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		for _, m := range reIndexName.FindAllSubmatch(src[loc[1]:end], -1) {
			out[name] = append(out[name], string(m[1]))
		}
	}
	return out
}

// TestIndexSpecsMatchMigration002 is the guard against the two drifting apart.
//
// They are separate code paths that must produce the same indexes: if a name
// differs, creating one after the other fails with IndexOptionsConflict and
// the service does not start. A live check of that costs a MongoDB; this one
// costs a file read, so it runs on every build.
func TestIndexSpecsMatchMigration002(t *testing.T) {
	fromJS := parseMigration002(t)
	fromGo := indexSpecs(30 * 24 * time.Hour)

	if len(fromGo) != len(fromJS) {
		t.Errorf("collection count: Go has %d, 002 has %d", len(fromGo), len(fromJS))
	}

	for coll, specs := range fromGo {
		want, ok := fromJS[coll]
		if !ok {
			t.Errorf("%s: indexed in Go but absent from 002", coll)
			continue
		}
		got := make([]string, 0, len(specs))
		for _, sp := range specs {
			got = append(got, sp.name)
		}
		if g, w := strings.Join(sortedCopy(got), ","), strings.Join(sortedCopy(want), ","); g != w {
			t.Errorf("%s index names:\n  Go:  %s\n  002: %s", coll, g, w)
		}
	}

	for coll := range fromJS {
		if _, ok := fromGo[coll]; !ok {
			t.Errorf("%s: in 002 but not in indexSpecs", coll)
		}
	}
}

// TestEveryIndexIsNamed is the invariant the struct exists to protect: an
// unnamed index gets a Mongo-derived name that will not match 002's.
func TestEveryIndexIsNamed(t *testing.T) {
	for coll, specs := range indexSpecs(30 * 24 * time.Hour) {
		for i, sp := range specs {
			if strings.TrimSpace(sp.name) == "" {
				t.Errorf("%s[%d]: index has no name", coll, i)
			}
			if len(sp.keys) == 0 {
				t.Errorf("%s[%d] (%s): index has no keys", coll, i, sp.name)
			}
		}
	}
}

// TestDeliveryTTLReachesTheIndex guards the one spec whose options depend on
// configuration: a TTL of zero would mean "never expire", silently unbounding
// the delivery ledger.
func TestDeliveryTTLReachesTheIndex(t *testing.T) {
	const ttl = 7 * 24 * time.Hour
	for _, sp := range indexSpecs(ttl)[CollDeliveries] {
		if sp.name != "receivedAt_ttl" {
			continue
		}
		if sp.ttl != ttl {
			t.Errorf("receivedAt_ttl ttl = %v, want %v", sp.ttl, ttl)
		}
		return
	}
	t.Fatal("receivedAt_ttl not found in CollDeliveries specs")
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
