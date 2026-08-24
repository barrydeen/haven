# Access Control: Whitelisting and Blacklisting

Haven allows you to manage who can interact with your relay through whitelisting and blacklisting.

## Whitelisting

Whitelisting grants specific npubs the same permissions as the relay owner. 

### Permissions granted to whitelisted npubs:
- **Outbox Publishing**: Ability to publish notes to your outbox relay.
- **Blossom Media Server**: Ability to upload to your Blossom server.
- **Private Relay Access**: Ability to read and write to your private relay (`/private`).
- **Web of Trust Bypass**: Whitelisted users are automatically trusted and do not need to be part of your Web of Trust 
  to interact with your Chat and Inbox relays.

### How to configure a Whitelist:
1. Create a JSON file (e.g., `whitelisted_npubs.json`) containing an array of npubs:
   ```json
   [
     "npub1...",
     "npub2..."
   ]
   ```
2. Set the `WHITELISTED_NPUBS_FILE` environment variable in your `.env` file to point to this file:
   ```env
   WHITELISTED_NPUBS_FILE=whitelisted_npubs.json
   ```

> [!NOTE]
> The relay owner's npub (defined by `OWNER_NPUB`) is automatically whitelisted. You don't need to add it to the file.

## Blacklisting

Blacklisting allows you to explicitly block specific npubs from interacting with your Haven relay, even if they would 
otherwise be allowed by the Web of Trust.

### Effects of Blacklisting:
- **Chat Relay**: Blacklisted users cannot send DMs or messages to your Chat relay.
- **Inbox Relay**: Your Inbox relay will reject notes from blacklisted users.
- **Import**: Events from blacklisted users will be skipped when importing from external relays (e.g., using 
 `./haven import` or from the live subscription to import relays).

> [!NOTE]
> Blacklisting does not affect Blossom Media Server access, Outbox publishing, or private relay access. In theory, you 
> could simultaneously whitelist and blacklist the same npub, which makes very little sense.

> [!IMPORTANT]
> Blacklisting has no effect when [importing JSONL files](backup.md#manual-restore).

### How to configure a Blacklist:
1. Create a JSON file (e.g., `blacklisted_npubs.json`) containing an array of npubs:
   ```json
   [
     "npub1...",
     "npub2..."
   ]
   ```
2. Set the `BLACKLISTED_NPUBS_FILE` environment variable in your `.env` file to point to this file:
   ```env
   BLACKLISTED_NPUBS_FILE=blacklisted_npubs.json
   ```

## Deleting Events

The relay owner (`OWNER_NPUB`) can delete **any** event stored on their relay by publishing a
[NIP-09](https://github.com/nostr-protocol/nips/blob/master/09.md) delete request (kind 5) to it, even when the event
was written by somebody else. Everybody else can only delete their own events.

A delete request applies to the relay it is sent to, so publish it to the endpoint that holds the event, for example:

```sh
nak event -k 5 -t e=<event-id> --sec <owner-nsec> wss://your.relay/inbox
```

Deletions stick: the delete request is kept, and the deleted event is refused if anybody tries to publish it again or
if it shows up while importing from your seed relays.

> [!NOTE]
> Deleting an event only removes it from your Haven relay. Copies on other relays are unaffected, though a delete
> request published to your outbox relay is blasted onwards like any other event.

---

[README](../README.md)
