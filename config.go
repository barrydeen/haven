package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/barrydeen/haven/pkg/nip05namecoin"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr/nip19"
)

type S3Config struct {
	AccessKeyID string `json:"access_key_id"`
	SecretKey   string `json:"secret_key"`
	Endpoint    string `json:"endpoint"`
	BucketName  string `json:"bucket_name"`
	Region      string `json:"region"`
}

type Config struct {
	OwnerNpub                            string              `json:"owner_npub"`
	OwnerPubKey                          string              `json:"owner_pubkey"`
	DBEngine                             string              `json:"db_engine"`
	LmdbMapSize                          int64               `json:"lmdb_map_size"`
	BlossomPath                          string              `json:"blossom_path"`
	RelayURL                             string              `json:"relay_url"`
	RelayPort                            int                 `json:"relay_port"`
	RelayBindAddress                     string              `json:"relay_bind_address"`
	RelaySoftware                        string              `json:"relay_software"`
	RelayVersion                         string              `json:"relay_version"`
	UserAgent                            string              `json:"user_agent"`
	PrivateRelayName                     string              `json:"private_relay_name"`
	PrivateRelayNpub                     string              `json:"private_relay_npub"`
	PrivateRelayDescription              string              `json:"private_relay_description"`
	PrivateRelayIcon                     string              `json:"private_relay_icon"`
	ChatRelayName                        string              `json:"chat_relay_name"`
	ChatRelayNpub                        string              `json:"chat_relay_npub"`
	ChatRelayDescription                 string              `json:"chat_relay_description"`
	ChatRelayIcon                        string              `json:"chat_relay_icon"`
	OutboxRelayName                      string              `json:"outbox_relay_name"`
	OutboxRelayNpub                      string              `json:"outbox_relay_npub"`
	OutboxRelayDescription               string              `json:"outbox_relay_description"`
	OutboxRelayIcon                      string              `json:"outbox_relay_icon"`
	InboxRelayName                       string              `json:"inbox_relay_name"`
	InboxRelayNpub                       string              `json:"inbox_relay_npub"`
	InboxRelayDescription                string              `json:"inbox_relay_description"`
	InboxRelayIcon                       string              `json:"inbox_relay_icon"`
	InboxPullIntervalSeconds             int                 `json:"inbox_pull_interval_seconds"`
	ImportStartDate                      string              `json:"import_start_date"`
	ImportOwnerNotesFetchTimeoutSeconds  int                 `json:"import_owned_notes_fetch_timeout_seconds"`
	ImportTaggedNotesFetchTimeoutSeconds int                 `json:"import_tagged_fetch_timeout_seconds"`
	ImportSeedRelays                     []string            `json:"import_seed_relays"`
	BackupProvider                       string              `json:"backup_provider"`
	BackupIntervalHours                  int                 `json:"backup_interval_hours"`
	WotDepth                             int                 `json:"wot_depth"`
	WotMinimumFollowers                  int                 `json:"wot_minimum_followers"`
	WotFetchTimeoutSeconds               int                 `json:"wot_fetch_timeout_seconds"`
	WotRefreshInterval                   time.Duration       `json:"wot_refresh_interval"`
	WhitelistedPubKeys                   map[string]struct{} `json:"whitelisted_pubkeys"`
	WhitelistedNamecoinNames             map[string]struct{} `json:"whitelisted_namecoin_names"`
	BlacklistedPubKeys                   map[string]struct{} `json:"blacklisted_pubkeys"`
	LogLevel                             string              `json:"log_level"`
	BlastrRelays                         []string            `json:"blastr_relays"`
	BlastrTimeoutSeconds                 int                 `json:"blastr_timeout_seconds"`
	S3Config                             *S3Config           `json:"s3_config"`
}

const relaySoftware = "https://github.com/barrydeen/haven"

func loadConfig() Config {
	_ = godotenv.Load(".env")

	cfg := Config{
		OwnerNpub:                            getEnv("OWNER_NPUB"),
		OwnerPubKey:                          nPubToPubkey(getEnv("OWNER_NPUB")),
		DBEngine:                             getEnvString("DB_ENGINE", "lmdb"),
		LmdbMapSize:                          getEnvInt64("LMDB_MAPSIZE", 0),
		BlossomPath:                          getEnvString("BLOSSOM_PATH", "blossom"),
		RelayURL:                             getEnv("RELAY_URL"),
		RelayPort:                            getEnvInt("RELAY_PORT", 3355),
		RelayBindAddress:                     getEnvString("RELAY_BIND_ADDRESS", "0.0.0.0"),
		RelaySoftware:                        relaySoftware,
		RelayVersion:                         getVersion(),
		UserAgent:                            fmt.Sprintf("Haven/%s (+%s)", getVersion(), relaySoftware),
		PrivateRelayName:                     getEnv("PRIVATE_RELAY_NAME"),
		PrivateRelayNpub:                     getEnv("PRIVATE_RELAY_NPUB"),
		PrivateRelayDescription:              getEnv("PRIVATE_RELAY_DESCRIPTION"),
		PrivateRelayIcon:                     getEnv("PRIVATE_RELAY_ICON"),
		ChatRelayName:                        getEnv("CHAT_RELAY_NAME"),
		ChatRelayNpub:                        getEnv("CHAT_RELAY_NPUB"),
		ChatRelayDescription:                 getEnv("CHAT_RELAY_DESCRIPTION"),
		ChatRelayIcon:                        getEnv("CHAT_RELAY_ICON"),
		OutboxRelayName:                      getEnv("OUTBOX_RELAY_NAME"),
		OutboxRelayNpub:                      getEnv("OUTBOX_RELAY_NPUB"),
		OutboxRelayDescription:               getEnv("OUTBOX_RELAY_DESCRIPTION"),
		OutboxRelayIcon:                      getEnv("OUTBOX_RELAY_ICON"),
		InboxRelayName:                       getEnv("INBOX_RELAY_NAME"),
		InboxRelayNpub:                       getEnv("INBOX_RELAY_NPUB"),
		InboxRelayDescription:                getEnv("INBOX_RELAY_DESCRIPTION"),
		InboxRelayIcon:                       getEnv("INBOX_RELAY_ICON"),
		InboxPullIntervalSeconds:             getEnvInt("INBOX_PULL_INTERVAL_SECONDS", 3600),
		ImportStartDate:                      getEnv("IMPORT_START_DATE"),
		ImportOwnerNotesFetchTimeoutSeconds:  getEnvInt("IMPORT_OWNER_NOTES_FETCH_TIMEOUT_SECONDS", 60),
		ImportTaggedNotesFetchTimeoutSeconds: getEnvInt("IMPORT_TAGGED_NOTES_FETCH_TIMEOUT_SECONDS", 120),
		ImportSeedRelays:                     getRelayListFromFile(getEnv("IMPORT_SEED_RELAYS_FILE")),
		BackupProvider:                       getEnvString("BACKUP_PROVIDER", "none"),
		BackupIntervalHours:                  getEnvInt("BACKUP_INTERVAL_HOURS", 24),
		WotDepth:                             getEnvInt("WOT_DEPTH", 3),
		WotMinimumFollowers:                  getEnvInt("WOT_MINIMUM_FOLLOWERS", 0),
		WotFetchTimeoutSeconds:               getEnvInt("WOT_FETCH_TIMEOUT_SECONDS", 30),
		WotRefreshInterval:                   getEnvDuration("WOT_REFRESH_INTERVAL", 24*time.Hour),
		WhitelistedPubKeys:                   getNpubsFromFile(getEnvString("WHITELISTED_NPUBS_FILE", "")),
		WhitelistedNamecoinNames:             getNamesFromFile(getEnvString("WHITELISTED_NAMECOIN_NAMES_FILE", "")),
		BlacklistedPubKeys:                   getNpubsFromFile(getEnvString("BLACKLISTED_NPUBS_FILE", "")),
		LogLevel:                             getEnvString("HAVEN_LOG_LEVEL", "INFO"),
		BlastrRelays:                         getRelayListFromFile(getEnv("BLASTR_RELAYS_FILE")),
		BlastrTimeoutSeconds:                 getEnvInt("BLASTR_TIMEOUT_SECONDS", 5),
		S3Config:                             getS3Config(),
	}

	// Relay owner is always whitelisted
	cfg.WhitelistedPubKeys[cfg.OwnerPubKey] = struct{}{}

	// Resolve any configured Namecoin (.bit) names into pubkeys and
	// merge them into the existing whitelist. Best-effort: failures
	// log a warning but do not abort startup, so an operator can bring
	// their pod online before the resolver succeeds.
	nip05namecoin.ResolveNamesToPubkeys(
		cfg.WhitelistedPubKeys,
		cfg.WhitelistedNamecoinNames,
		nip05namecoin.ResolveOptions{Logger: log.Default()},
	)

	return cfg

}

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	return info.Main.Version
}

func getS3Config() *S3Config {
	backupProvider := getEnvString("BACKUP_PROVIDER", "none")

	if backupProvider == "s3" {
		return &S3Config{
			AccessKeyID: getEnv("S3_ACCESS_KEY_ID"),
			SecretKey:   getEnv("S3_SECRET_KEY"),
			Endpoint:    getEnv("S3_ENDPOINT"),
			BucketName:  getEnv("S3_BUCKET_NAME"),
			Region:      getEnv("S3_REGION"),
		}
	}

	return nil
}

func getRelayListFromFile(filePath string) []string {
	file, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("Failed to read file: %s", err)
	}

	var relayList []string
	if err := json.Unmarshal(file, &relayList); err != nil {
		log.Fatalf("Failed to parse JSON: %s", err)
	}

	for i, relay := range relayList {
		relay = strings.TrimSpace(relay)
		if !strings.HasPrefix(relay, "wss://") && !strings.HasPrefix(relay, "ws://") {
			relay = "wss://" + relay
		}
		relayList[i] = relay
	}
	return relayList
}

// getNamesFromFile loads a JSON array of Namecoin (.bit) identifiers
// from `filePath` and returns them as a set. Identifiers are kept as
// strings (e.g. "me@me.bit", "d/me") and resolved later at startup.
// Mirrors getNpubsFromFile: parse failures abort startup, since the
// operator clearly intended to load a whitelist file.
func getNamesFromFile(filePath string) map[string]struct{} {
	names, err := nip05namecoin.LoadNamesFile(filePath)
	if err != nil {
		log.Fatalf("Failed to load Namecoin names file: %s", err)
	}
	return names
}

func getNpubsFromFile(filePath string) map[string]struct{} {
	pubKeys := map[string]struct{}{}
	if filePath == "" {
		// No pubKeys file, only owner will be whitelisted"
		return pubKeys
	}
	file, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("Failed to read file: %s", err)
	}

	var npubs []string
	if err := json.Unmarshal(file, &npubs); err != nil {
		log.Fatalf("Failed to parse JSON: %s", err)
	}

	for _, npub := range npubs {
		npub = strings.TrimSpace(npub)
		pubKeys[nPubToPubkey(npub)] = struct{}{}
	}
	return pubKeys
}

func getEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		log.Fatalf("Environment variable %s not set", key)
	}
	return value
}

func getEnvString(key string, defaultValue string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, ok := os.LookupEnv(key); ok {
		intValue, err := strconv.Atoi(value)
		if err != nil {
			panic(err)
		}
		return intValue
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value, ok := os.LookupEnv(key); ok {
		intValue, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			panic(err)
		}
		return intValue
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		boolValue, err := strconv.ParseBool(value)
		if err != nil {
			panic(err)
		}
		return boolValue
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value, ok := os.LookupEnv(key); ok {
		durationValue, err := time.ParseDuration(value)
		if err != nil {
			panic(err)
		}
		return durationValue
	}
	return defaultValue
}

func nPubToPubkey(nPub string) string {
	_, v, err := nip19.Decode(nPub)
	if err != nil {
		panic(err)
	}
	return v.(string)
}

var art = `
██╗  ██╗ █████╗ ██╗   ██╗███████╗███╗   ██╗
██║  ██║██╔══██╗██║   ██║██╔════╝████╗  ██║
███████║███████║██║   ██║█████╗  ██╔██╗ ██║
██╔══██║██╔══██║╚██╗ ██╔╝██╔══╝  ██║╚██╗██║
██║  ██║██║  ██║ ╚████╔╝ ███████╗██║ ╚████║
╚═╝  ╚═╝╚═╝  ╚═╝  ╚═══╝  ╚══════╝╚═╝  ╚═══╝
HIGH AVAILABILITY VAULT FOR EVENTS ON NOSTR
	`
