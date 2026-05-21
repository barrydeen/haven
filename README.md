# HAVEN

[![Go version](https://img.shields.io/github/go-mod/go-version/barrydeen/haven?logo=go)](./go.mod#L3)
[![GitHub Release](https://img.shields.io/github/v/release/barrydeen/haven?link=https%3A%2F%2Fgithub.com%2Fbarrydeen%2Fhaven%2Freleases%2Flatest)](https://github.com/barrydeen/haven/releases/latest)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![CI](https://github.com/barrydeen/haven/actions/workflows/lint.yml/badge.svg)](https://github.com/barrydeen/haven/actions/workflows/lint.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/barrydeen/haven)](https://goreportcard.com/report/github.com/barrydeen/haven)

> [!IMPORTANT]
> HAVEN is considered feature complete and, going forward, Barry Deen's repository will only receive bug fixes.
> See the announcement [here](https://jumble.social/notes/nevent1qvzqqqqqqypzpckv7l8jqspl8u4y54dn9rcduwlrs4v2040nxce0m2h0cunvrj8tqy88wumn8ghj7mn0wvhxcmmv9uq3wamnwvaz7tmjv4kxz7fwwpexjmtpdshxuet59uqzqnjwq82z3lq62mkalaxu2dlgnjxw2stcwxan9wl66s7eywwjljvqx0s8cp)

HAVEN (High Availability Vault for Events on Nostr) is the most sovereign personal relay for the Nostr protocol, for 
storing and backing up sensitive notes like eCash, private chats, and drafts. It is a relay that is not so dumb, with 
features like web of trust, whitelisting, blacklisting, JSONL backup / restore (including periodic backups to the 
cloud), blastr note to others relays, and importing your old notes from other relays. It even includes its own 
blossom media server!

## Four Relays in One + Blossom Media Server

**Private Relay**: This relay is only accessible by the owner of the relay and whitelisted npubs. It is used for 
drafts, ecash, and other private notes that nobody else can read or write to. It is protected by Auth.

**Chat Relay**: This relay is used to contact the owner of the relay and whitelisted npubs by DM. Only people in the 
web of trust can interact with this relay, protected by Auth. It only accepts encrypted DMs and group chat kinds.

**Inbox Relay**: This relay is where the owner of the relay and whitelisted npubs reads from. Send your zaps, 
reactions, and replies to this relay when you're tagging the owner or one of the whitelisted npubs. You can also pull 
notes from this relay if you want notes where the owner or whitelisted npub are tagged. This relay automatically pulls 
notes from other relays. Only notes where the owner or whitelisted npubs are tagged will be accepted to this relay.

**Outbox Relay**: This relay is where the owner's and whitelisted npubs notes all live and are publicly accessible. You 
can import all your old notes to this relay. All notes sent to this relay are blasted to other relays. Only the 
owner and whitelisted npubs can send to this relay, but anyone can read.

**Blossom Media Server**: This relay also includes a media server for hosting images and videos. You can upload images 
and videos to this relay and get a link to share them. Only the relay owner and whitelisted npubs can upload to this 
relay, but anyone can view the images and videos.

## Not So Dumb Relay Features

**Web of Trust**: Protected from DM and Inbox spam by using a Web of Trust (WoT). See the [Web of Trust 
Documentation](docs/wot.md) for more details.

**Access Control**: Whitelist and blacklist npubs. See the [Access Control Documentation](docs/access-control.md) 
for more details.

**Inbox Relay**: Notes are pulled from other relays and stored in the inbox relay.

**Blastr**: Notes sent to the outbox are also blasted to other relays.

**Import Old Notes**: Import your old notes and notes you're tagged in from other relays.

**Backup/Recover**: It is your data, manually export or import data JSONL at any time. Set periodic backups to the cloud 
for easy recovery. See [Backup Documentation](docs/backup.md) for more details.

## Installation

### Option 1: Download Pre-built Binaries (Recommended)

The easiest way to get started with Haven is to download pre-built binaries from our GitHub releases page:

**[Download Haven Releases](https://github.com/barrydeen/haven/releases/)**

#### Installation Steps:

1. **Download the appropriate binary** for your system from the releases page
2. **Verify the download (optional)**: See our [Verification Documentation](docs/verify.md) for 
instructions on how to verify the authenticity of the binaries using GPG signatures and checksums.
3. **Create a haven directory** and extract the downloaded file:
   ```bash
   mkdir haven
   # For Linux/macOS:
   tar -xzf haven_[Platform]_[Architecture].tar.gz -C haven
   # For Windows: extract the .zip file to this directory
   ```
### Option 2: Build from Source

If you prefer to build Haven from source or need to customize the build, please see the [BuildDocumentation](docs/build.md).

### Option 3:

Check out some of the external community-built tools for managing Haven relays:

1. [**Haven Docker**](https://github.com/HolgerHatGarKeineNode/haven-docker): A lightweight Docker Compose setup 
   with a TUI for configuration and management
2. [**HAVEN For Mac**](https://github.com/btcforplebs/haven-mac): A user-friendly macOS application for managing 
   your Haven relay with a native GUI and a ton of extra features
3. [**Haven Start9 Wrapper**](https://github.com/higedamc/haven-start9-wrapper): A Tor-only Start9 wrapper for Haven 
   with its own Dashboard and Web UI
4. [**HAVEN Kit**](https://github.com/Letdown2491/haven-kit): Simple configuration tool to set up a HAVEN Nostr 
   relay in Umbrel Docker or Podman with just a few clicks 

If you have built a Haven relay management tool that you would like to share, please open a PR to add it to this list!

> [!NOTE]
> The Haven team does not officially support the tools listed above. They are community-built and maintained by 
> third parties. Please refer to the respective repositories for installation instructions, documentation, and 
> support. If you encounter any issues with these tools, please open an issue in their respective repositories.

## Setup Instructions

Follow these steps to get the Haven Relay running on your local machine (after installing via binary download or building from source):

### 1. Copy `.env.example` to `.env`

You'll need to create an `.env` file based on the example provided.

```bash
cp .env.example .env
```

### 2. Set your environment variables

Open the `.env` file and set the necessary environment variables.

### 3. Create the relays JSON files

Copy the example relays JSON files for your seed and blastr relays:

```bash
cp relays_import.example.json relays_import.json
```

```bash
cp relays_blastr.example.json relays_blastr.json
```

Customise the list of relays above as needed for your setup. The JSON should contain an array of relay URLs, which
default to wss:// if you don't explicitly specify the protocol.

> [!TIP]
> It is hard to keep an up-to-date list of working Nostr relays. Keep an eye on Haven's logs to see if any of the 
> relays you're using are no longer working and replace them accordingly. There is no guarantee that the example relays 
> will be up and running at all times (or at all), and it is up to the user to keep their relay list up to date.


### 4. Configure Access Control (optional)

Haven allows you to whitelist specific npubs to grant them full relay access or blacklist them to prevent any 
interaction with your relay. 

See the [Access Control Documentation](docs/access-control.md) for more details on how to set up whitelists and blacklists.

#### Whitelisting by Namecoin (`.bit`) name

As an additive opt-in alternative, you can whitelist users by their Namecoin (`.bit`) name instead of (or in
addition to) their npub. Point `WHITELISTED_NAMECOIN_NAMES_FILE` at a JSON file containing `.bit` identifiers
(e.g. `me@me.bit`, `d/example`, `id/alice`). At startup, Haven resolves each name to its underlying pubkey via
public Namecoin ElectrumX servers and merges the result into the whitelist alongside `WHITELISTED_NPUBS_FILE`.

Resolution is best-effort: if a name can't be resolved (no Namecoin record, ElectrumX unreachable, etc.) Haven
logs a warning and keeps starting. If both `WHITELISTED_NPUBS_FILE` and `WHITELISTED_NAMECOIN_NAMES_FILE` are
set, both contribute to the same in-memory whitelist. See `whitelisted_namecoin_names.example.json` for the
file shape. Spec reference: [NIP-05 Namecoin extension](https://github.com/nostr-protocol/nips/pull/2349).

### 5. Run on System Startup

#### Linux – Create a Systemd Service

<details><summary>Click here to view the installation routine for Systemd</summary>
<p>

To have the relay run as a service, create a systemd unit file. Make sure to limit the memory usage to less than your 
system's total memory to prevent the relay from crashing the system. Replace the values for `ExecStart` and
`WorkingDirectory` with the actual paths where you installed Haven and stored the `.env` file.

1. Create the file:

   ```bash
   sudo nano /etc/systemd/system/haven.service
   ```

2. Add the following contents:

   ```ini
   [Unit]
   Description=Haven Relay
   After=network.target
   
   [Service]
   ExecStart=/home/ubuntu/haven/haven #Edit path to point to where you installed Haven
   WorkingDirectory=/home/ubuntu/haven #Edit path to point to where you installed Haven
   MemoryMax=1000M  # Example, Limit memory usage to 1000 MB | Edit this to fit your machine
   Restart=always
   
   [Install]
   WantedBy=multi-user.target
   ```

3. Reload systemd to recognize the new service:

   ```bash
   sudo systemctl daemon-reload
   ```

4. Start the service:

   ```bash
   sudo systemctl start haven
   ```

5. (Optional) Enable the service to start on boot:

   ```bash
   sudo systemctl enable haven
   ```

</p>
</details>

#### macOS - Create a login item App

<details><summary>Click here to view the installation routine for macOS</summary>
<p>

To have the relay run on boot, create a script that will open the terminal and run the haven binary, the terminal will 
remain open and the relay running with it. Be sure the download `/haven` directory is located in the macOS home 
folder `~/`

1. Create the App: Open Script Editor

2. Add the following contents:

   ```ini
   tell application "Terminal"
     activate
     do script "cd \"$HOME/haven\"; ./haven; exec $SHELL"
   end tell
   ```
3. Save in the Applications folder

4. Open System - Settings - General - Login Items
   Hit the plus, add run_haven from the Applications folder

5. Reboot – On initial restart and terminal auto-open, choose “allow”

6. Reboot again to test the login item

</p>
</details>

### 6. Set up a Reverse Proxy (optional)

To have a domain name (example: relay.domain.com) point to your machine, you will need to set up a reverse proxy.

#### Nginx

<details><summary>Click here to view the installation routine for Nginx</summary>
<p>

1. Install nginx on your relay:

   ```bash
   sudo apt-get update 
   sudo apt-get install nginx
   ```

2. Remove default config: `sudo rm -rf /etc/nginx/sites-available/default`

3. Create new default config: `sudo nano /etc/nginx/sites-available/default` 

4. Add a new reverse proxy config by adding the following configuration to your nginx configuration file:

```nginx
server {
    listen 80;
    server_name yourdomain.com;
    client_max_body_size 100m;

    location / {
        proxy_pass http://localhost:3355;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

Replace `yourdomain.com` with your actual domain name.

> **Note:**
> [`client_max_body_size`](https://nginx.org/en/docs/http/ngx_http_core_module.html#client_max_body_size) is set to 
> `100m` to allow for larger media files to be uploaded to Blossom. `0` can be used to allow for unlimited file 
> sizes. If you are using Cloudflare proxy, be mindful of 
> [upload limits](https://community.cloudflare.com/t/maximum-upload-size-is-limit/418490/2).

After adding the configuration, restart nginx:

```bash
sudo systemctl restart nginx
```

</p>
</details>

#### Apache

<details><summary>Click here to view the installation routine for Apache</summary>
<p>

1. Install Apache on your server
2. Set up a virtual host:
   ```apache
   <VirtualHost *:80>
           ServerName yourdomain.com
   
           RewriteEngine On
           RewriteCond %{HTTP:Upgrade} websocket [NC]
           RewriteCond %{HTTP:Connection} upgrade [NC]
           RewriteRule ^/?(.*) "ws://localhost:3355/$1" [P,L]
   
           # Proxy for HTTP traffic (NIP-11 relay info page)
           ProxyPass / http://localhost:3355/
           ProxyPassReverse / http://localhost:3355/
   
           # Optional: Add HSTS header for enhanced security
           Header always set Strict-Transport-Security "max-age=63072000; includeSubDomains; preload"
   
           # Optional: Set appropriate WebSocket headers
           RequestHeader set Upgrade "websocket"
           RequestHeader set Connection "Upgrade"
   </VirtualHost>
   ```

Replace `yourdomain.com` with your actual domain name.

```bash
sudo systemctl restart httpd
```

</p>
</details>

#### Caddy
<details><summary>Click here to view the installation routine for Caddy</summary>
<p>

Preparation: Set the A record (for your domain) to point to the server's IP address.

1. Install caddy:

   ```bash
   sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https
   curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
   curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
   sudo apt update
   sudo apt install caddy
   ```

2. Open Caddyfile:

   ```bash
   sudo nano /etc/caddy/Caddyfile
   ```

3. Add configuration:

   ```bash
   # Configuration for HAVEN Relay
   yourdomain.com {
       reverse_proxy localhost:3355 {
           header_up Host {host}
           header_up X-Real-IP {remote_host}
           header_up X-Forwarded-For {remote_host}
           header_up X-Forwarded-Proto {scheme}
           transport http {
               versions 1.1
           }
       }
       request_body {
           max_size 100MB
       }
   }
   ```

4. Reload Caddy:

   ```bash
   sudo systemctl reload caddy
   ```

5. Check status logs:

```bash
sudo systemctl status caddy
sudo journalctl -u caddy -f --since "2 hour ago"
```

**Note:** Caddy automatically manages certificates and WebSocket connections. Certbot is not required.

</p>
</details>

### 7. Install Certbot (optional)

If you want to serve the relay over HTTPS, you can use Certbot to generate an SSL certificate.

```bash
sudo apt-get update
sudo apt-get install certbot python3-certbot-nginx
```

After installing Certbot, run the following command to generate an SSL certificate:

```bash
sudo certbot --nginx
```

Apache:

```bash
sudo certbot --apache
```

Follow the instructions to generate the certificate.

> [!NOTE]
> The `cerbot` command will fail if the Domain you added to nginx is not yet pointing at your machine's IP address. 
This is done by adding an A record subdomain pointing to your IP address through your DNS recrods Manager.

### 8. Import your old notes (optional)

If you want to import your old notes and notes you're tagged in from other relays, run the following command:

```bash
sudo systemctl stop haven
./haven import
sudo systemctl start haven
```

### 9. Access the relay

Once everything is set up, the relay will be running on `localhost:3355` with the following endpoints:

- `localhost:3355` (outbox and Blossom server)
- `localhost:3355/private`
- `localhost:3355/chat`
- `localhost:3355/inbox`

## Database

Haven currently supports [BadgerDB](https://github.com/dgraph-io/badger) and [LMDB](https://www.symas.com/mdb) as embedded
databases, meaning no external database is required.

By default, Haven uses BadgerDB. To switch to LMDB, set the `DB_ENGINE` environment variable to `lmdb` in the `.env` file.

LMDB can be faster than BadgerDB but performs best with NVMe drives and may require fine-tuning based on factors such as
database size, operating system, file system, and hardware.

### LMDB Map Size

There is no one-size-fits-all value for LMDB’s map size. Windows and macOS users, in particular, may need
to adjust the `LMDB_MAPSIZE` environment variable to a value lower than the available free disk space if the default
value of 273 GB is too high. Otherwise, Haven will fail to bootstrap. Users with large databases may also need to
increase the `LMDB_MAPSIZE` value above the default. On most systems, the default value should work fine.

Despite the large default value, on most modern systems LMDB will only use the disk space it needs. The map size simply
defines an upper limit for the database size. For more information about LMDB’s map size, refer to the
[LMDB documentation](http://www.lmdb.tech/doc/group__mdb.html#gaa2506ec8dab3d969b0e609cd82e619e5).

### Migrating from databases created in older versions of Haven

Haven uses [Khatru's event store](https://github.com/fiatjaf/eventstore) to store notes. The way events are stored evolves 
over time, and occasionally this introduces breaking changes.

As a precaution, before upgrading to a newer version of Haven, you should back up the `db` folder.

Haven versions 1.0.3 and earlier did not replace outdated notes. While this does not affect the relay's core
functionality, it can result in a bloated database, reduced performance, and bugs in some clients. For this reason, it
is recommended to delete old databases and start fresh.

BadgerDB users upgrading from Haven version 1.0.5 or earlier may encounter a critical error when starting the relay:

```
error running migrations: failed to delete index key xxxx: Txn is too big to fit into one request
```

As a workaround, you can delete the `db` folder and start fresh, optionally [re-importing](#8-import-your-old-notes-optional) your
previous notes.

## Blossom Media Server

The outbox relay also functions as a media server for hosting images and videos. You can upload media files to the relay 
and get a shareable link. Only the relay owner and whitelisted npubs have upload permissions to the media server, 
but anyone can view the hosted images and videos.

Media files are stored in the file system based on the `BLOSSOM_PATH` environment variable set in the `.env` file. 
The default path is `./blossom`.

## Cloud Backups

Haven can back up and restore your notes using a portable JSONL format. This can be done either with the built-in
`./haven backup` and `./haven restore` commands, or with a scheduled backup periodically uploaded to your cloud 
storage.

See [Backup Documentation](docs/backup.md#periodic-cloud-backups) and
[Cloud Storage Provider Specific Instructions](docs/cloud-storage.md) for further details.

## License

This project is licensed under the [MIT](./LICENSE) License.
