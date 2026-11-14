package main

import (
	"os"
	"testing"
)

// TestMain seeds the environment loadConfig insists on and loads the package
// config before any test runs. loadConfig does not run at package-var time
// (main.go loads it in main()): go test always presents non-test files before
// test files, so a package-var load would exit inside init with
// "Environment variable OWNER_NPUB not set" before any test could run.
//
// loadConfig reads only the process environment: it never consults the relay's
// own .env, so tests behave identically regardless of what a maintainer has on
// disk.
func TestMain(m *testing.M) {
	seedTestEnv()
	config = loadConfig()
	os.Exit(m.Run())
}

func seedTestEnv() {
	env := map[string]string{
		"OWNER_NPUB":                "npub1utx00neqgqln72j22kej3ux7803c2k986henvvha4thuwfkper4s7r50e8",
		"RELAY_URL":                 "localhost:3355",
		"PRIVATE_RELAY_NAME":        "test",
		"PRIVATE_RELAY_NPUB":        "npub1utx00neqgqln72j22kej3ux7803c2k986henvvha4thuwfkper4s7r50e8",
		"PRIVATE_RELAY_DESCRIPTION": "test",
		"PRIVATE_RELAY_ICON":        "",
		"CHAT_RELAY_NAME":           "test",
		"CHAT_RELAY_NPUB":           "npub1utx00neqgqln72j22kej3ux7803c2k986henvvha4thuwfkper4s7r50e8",
		"CHAT_RELAY_DESCRIPTION":    "test",
		"CHAT_RELAY_ICON":           "",
		"OUTBOX_RELAY_NAME":         "test",
		"OUTBOX_RELAY_NPUB":         "npub1utx00neqgqln72j22kej3ux7803c2k986henvvha4thuwfkper4s7r50e8",
		"OUTBOX_RELAY_DESCRIPTION":  "test",
		"OUTBOX_RELAY_ICON":         "",
		"INBOX_RELAY_NAME":          "test",
		"INBOX_RELAY_NPUB":          "npub1utx00neqgqln72j22kej3ux7803c2k986henvvha4thuwfkper4s7r50e8",
		"INBOX_RELAY_DESCRIPTION":   "test",
		"INBOX_RELAY_ICON":          "",
		"IMPORT_START_DATE":         "2023-01-20",
		"IMPORT_SEED_RELAYS_FILE":   "relays_import.example.json",
		"BLASTR_RELAYS_FILE":        "relays_blastr.example.json",
	}
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
}
