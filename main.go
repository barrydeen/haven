package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/joho/godotenv"
	"github.com/spf13/afero"

	"github.com/barrydeen/haven/pkg/wot"
)

var (
	pool   *nostr.Pool
	config Config
	fs     afero.Fs
)

func main() {
	// loaded here, not at package-var time: a test binary would hit loadConfig's
	// log.Fatalf before TestMain gets a chance to seed the environment
	//
	// .env seeding stays out of loadConfig, which reads only the process env:
	// a maintainer's .env must not decide what tests do
	_ = godotenv.Load(".env") // missing file is a clean checkout's normal state
	config = loadConfig()

	// before initRelays/initDBs open the backends: this re-encodes raw event
	// blobs written by the deprecated eventstore codec in place, and drops the
	// successor backend's version so its own reindex runs on the next Init. A
	// failure here must not stop the relay from booting: new events would still
	// store fine, only legacy history would be unreadable.
	if _, err := migrateLegacyRawEvents([]string{"db/private", "db/chat", "db/outbox", "db/inbox", "db/blossom"}); err != nil {
		log.Printf("legacy lmdb migration failed (continuing): %v", err)
	}

	nostr.InfoLogger = log.New(io.Discard, "", 0)
	slog.SetLogLoggerLevel(getLogLevelFromConfig())
	green := "\033[32m"
	reset := "\033[0m"
	fmt.Println(green + art + reset)

	mainCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs = afero.NewOsFs()
	if err := fs.MkdirAll(config.BlossomPath, 0755); err != nil {
		log.Fatal("🚫 error creating blossom path:", err)
	}
	checkBlobPath()

	// before the subcommand switch below, so backup, restore and import get the
	// same view of who is banned and allowed as the relay does
	loadManagementStore()

	pool = nostr.NewPool()
	pool.RelayOptions = nostr.RelayOptions{
		RequestHeader: http.Header{
			"User-Agent": []string{config.UserAgent},
		},
	}
	pool.StartPenaltyBox()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			runBackup(mainCtx)
			return
		case "restore":
			runRestore(mainCtx)
			return
		case "import":
			ensureImportRelays()
			runImport(mainCtx)
			return
		case "help":
			printHelp()
			return
		}

		if os.Args[1] == "-h" || os.Args[1] == "--help" {
			printHelp()
			return
		}
	}

	flag.Parse()

	log.Println("🚀 HAVEN", config.RelayVersion, "is booting up")
	defer log.Println("🔌 HAVEN is shutting down")
	log.Println("👥 Number of whitelisted pubkeys:", len(whitelistedPubKeySet()))
	log.Println("🚷 Number of blacklisted pubkeys:", len(config.BlacklistedPubKeys))

	ensureImportRelays()
	wotModel := wot.NewSimpleInMemory(
		pool,
		whitelistedPubKeySet,
		config.ImportSeedRelays,
		config.WotDepth,
		config.WotMinimumFollowers,
		config.WotFetchTimeoutSeconds,
	)
	wot.Initialize(mainCtx, wotModel)
	initRelays(mainCtx)

	// after initRelays, because instrument() is what creates the per relay
	// counters this reads a history into, and before the goroutines below so the
	// first fold has something to fold onto
	loadMetricsStore()

	go func() {
		go subscribeInboxAndChat(mainCtx)
		go startPeriodicCloudBackups(mainCtx)
		go wot.PeriodicRefresh(mainCtx, config.WotRefreshInterval)
		go runMetrics(mainCtx)
		go runAggregates(mainCtx)
	}()

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("templates/static"))))
	http.HandleFunc("/admin", adminHandler)
	// without this "/admin/" would fall through to the catch-all below, reach
	// the outbox relay and come back as a bare 404
	http.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})
	http.HandleFunc("/", dynamicRelayHandler)

	addr := fmt.Sprintf("%s:%d", config.RelayBindAddress, config.RelayPort)

	log.Printf("🔗 listening at %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("🚫 error starting server:", err)
	}
}

func printHelp() {
	fmt.Println("haven is a personal nostr relay.")
	fmt.Println()
	fmt.Println("usage: haven [command]")
	fmt.Println()
	fmt.Println("commands:")
	fmt.Println("  backup  - backup the database")
	fmt.Println("  restore - restore the database")
	fmt.Println("  import  - import notes from seed relays")
	fmt.Println("  help    - show this help message")
	fmt.Println()
	fmt.Println("if no command is provided, the relay starts by default.")
	fmt.Println()
	fmt.Println("run 'haven [command] --help' for more information on a command.")
}

func dynamicRelayHandler(w http.ResponseWriter, r *http.Request) {
	relay, relayName, exact := relayForPath(r.URL.Path)

	// NIP-86 is answered ahead of khatru: khatru reports auth failures as HTTP
	// 200 where the NIP asks for a 401, never checks the auth event's kind or
	// method tag, and has no way to tell haven's four relays apart
	if isNIP86Request(r) {
		handleManagementRequest(w, r, relay, relayName, exact)
		return
	}

	relay.ServeHTTP(w, r)
}

// relayForPath maps a request path onto one of the four relays. exact reports
// whether the path names that relay outright: everything unmatched lands on the
// outbox relay, which also serves blossom, so a caller that needs to know it is
// really addressing a relay — NIP-86 does, it lives on the relay's own URI and
// nowhere else — has to ask.
func relayForPath(path string) (*khatru.Relay, string, bool) {
	// trailing slashes used to fall through to the outbox relay, so "/private/"
	// silently served the wrong one
	switch strings.TrimSuffix(path, "/") {
	case "/private":
		return privateRelay, relayPrivate, true
	case "/chat":
		return chatRelay, relayChat, true
	case "/inbox":
		return inboxRelay, relayInbox, true
	case "":
		return outboxRelay, relayOutbox, true
	default:
		return outboxRelay, relayOutbox, false
	}
}

func getLogLevelFromConfig() slog.Level {
	switch config.LogLevel {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo // Default level
	}
}
