package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Seed with the previous release's dependency, then migrate and reopen with
// this release. Only generated test data is used; no production database or
// credentials are copied. Opt in because seeding builds an older Go module.
func TestPocketBaseUpgradePreservesRecordsAndIndexes(t *testing.T) {
	if os.Getenv("BESZEL_TEST_PB_UPGRADE") != "1" {
		t.Skip("set BESZEL_TEST_PB_UPGRADE=1 to build the prior-version database fixture")
	}
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("probe.go", pocketbaseUpgradeProbe)
	write("old.mod", "module beszel-upgrade-fixture\n\ngo 1.26.8\n\nrequire github.com/pocketbase/pocketbase v0.37.4\n")
	dataDir := filepath.Join(root, "data")
	run := func(phase string, old bool) {
		t.Helper()
		args := []string{"run"}
		if old {
			args = append(args, "-mod=mod", "-modfile="+filepath.Join(root, "old.mod"))
		} else {
			args = append(args, "-mod=readonly")
		}
		args = append(args, filepath.Join(root, "probe.go"), phase, dataDir)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		output, err := exec.CommandContext(ctx, "go", args...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), phase+" passed") {
			t.Fatalf("PocketBase %s probe failed: %v: %s", phase, err, output)
		}
	}
	run("seed", true)
	run("verify", false)
	run("verify", false)
	t.Log("PocketBase 0.37.4 data migrated to the current version; records, configured/external indexes and transactional writes survived two startups")
}

const pocketbaseUpgradeProbe = `package main
import (
    "fmt"
    "os"
    "strings"
    "github.com/pocketbase/pocketbase/core"
    _ "github.com/pocketbase/pocketbase/migrations"
)
func must(err error) { if err != nil { panic(err) } }
func main() {
    phase, dir := os.Args[1], os.Args[2]
    app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir})
    must(app.Bootstrap())
    defer app.ResetBootstrapState()
    const table = "upgrade_fixture"
    if phase == "seed" {
        collection := core.NewBaseCollection(table)
        collection.Fields.Add(&core.TextField{Name:"hostname"}, &core.NumberField{Name:"sequence"})
        collection.Indexes = []string{"CREATE INDEX idx_fixture_hostname ON upgrade_fixture (hostname)"}
        must(app.Save(collection))
        record := core.NewRecord(collection)
        record.Id = "seedrecord00001"
        record.Set("hostname", "node-before")
        record.Set("sequence", 1)
        must(app.Save(record))
        _, err := app.DB().NewQuery("CREATE UNIQUE INDEX idx_external_sequence ON upgrade_fixture (sequence)").Execute()
        must(err)
    } else {
        collection, err := app.FindCollectionByNameOrId(table)
        must(err)
        for _, name := range []string{"idx_fixture_hostname", "idx_external_sequence"} {
            found := false
            for _, raw := range collection.Indexes { found = found || strings.Contains(raw,name) }
            if !found { panic("collection lost index "+name) }
            var count int
            must(app.DB().NewQuery("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='"+name+"'").Row(&count))
            if count != 1 { panic("SQLite lost index "+name) }
        }
        record, err := app.FindFirstRecordByFilter(table, "hostname = 'node-before'")
        must(err)
        if record.Id != "seedrecord00001" { panic("record identity changed during migration") }
        if record.GetFloat("sequence") != 1 && record.GetFloat("sequence") != 2 { panic("record changed during migration") }
        must(app.RunInTransaction(func(tx core.App) error {
            record.Set("sequence",2)
            if err := tx.Save(record); err != nil { return err }
            records, err := tx.FindRecordsByFilter(table,"hostname = 'node-after'","",1,0)
            if err != nil { return err }
            if len(records) == 0 {
                next := core.NewRecord(collection)
                next.Set("hostname","node-after")
                next.Set("sequence",3)
                return tx.Save(next)
            }
            return nil
        }))
        var count int
        must(app.DB().NewQuery("SELECT count(*) FROM upgrade_fixture").Row(&count))
        if count != 2 { panic("records changed after transaction") }
    }
    fmt.Println(phase+" passed")
}
`
