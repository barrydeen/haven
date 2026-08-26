package main

import "os"

// seedTestEnv fills in the environment loadConfig insists on before the package
// level `config = loadConfig()` in config.go gets to run.
//
// It is a variable rather than an init() on purpose: package level variables are
// initialised before init() functions, and files are processed in sorted order,
// so this file's name is what puts it ahead of config.go. That is load bearing —
// renaming this file breaks every test in the package with a log.Fatalf about
// OWNER_NPUB rather than a test failure.
//
// The relay's own .env is not used: a test that behaved differently on the
// maintainer's machine than in CI would be worse than no test.
var _ = seedTestEnv()

func seedTestEnv() bool {
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
		os.Setenv(k, v)
	}
	return true
}
