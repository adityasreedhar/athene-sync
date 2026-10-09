# Athene sync server

Keeps Athene, the reading app for iPhone and iPad, and KOReader on your Kobo, Kindle, Boox or PocketBook in
step: your books, highlights, notes, bookmarks and the place you've reached, both ways. It runs on
a computer of yours that stays on (a NAS, a Raspberry Pi), so your reading never passes through
anyone else's server.

It is also a complete KOReader Progress sync server: KOReader's own *Progress sync* works with it
unchanged.

[Athene on the App Store](https://apps.apple.com/app/id6811711454) ·
[athenereader/athene-sync on Docker Hub](https://hub.docker.com/r/athenereader/athene-sync)

## Run it

On a computer that stays on, with [Docker](https://docs.docker.com/get-started/get-docker/)
installed, run this in a terminal:

```sh
docker run -d --name athene-sync --restart unless-stopped \
  -p 17200:17200 -v athene-sync:/data athenereader/athene-sync
```

Your server's address is `http://` + that computer's address + `:17200`, for example
`http://192.168.1.20:17200`. Open it in a browser for the next steps.

On a NAS you can add the image in its Docker or Container Manager app instead: publish port
**17200**, and map a folder to `/data` so your accounts and books survive updates.

To reach it from outside your home, put it behind a reverse proxy with a real certificate.

| Setting | Default | |
|---|---|---|
| `ATHENE_LISTEN` | `:17200` | Where it listens. |
| `ATHENE_DATA` | `/data` | Accounts, books and annotations. |
| `ATHENE_REGISTRATION` | `true` | Set to `false` once your accounts are made. |
| `ATHENE_MAX_BOOK_MB` | `512` | The largest book Athene may send. |

## Set up KOReader and Athene

New to KOReader? [Install it on your e-reader first](https://github.com/koreader/koreader#installation).

1. **The plugin:** download it from `http://<server>:17200/plugin` and unzip it. Plug in your
   e-reader and copy the `athene.koplugin` folder into KOReader's `plugins` folder: on a Kobo
   `.adds/koreader/plugins`, on a Kindle `koreader/plugins`, on a PocketBook
   `applications/koreader/plugins`, on a Boox or Android `koreader/plugins`. A Mac hides folders whose
   names start with a dot: press ⌘⇧. in Finder to see them. Eject the e-reader, then restart
   KOReader: tap the top of the screen and choose *☰ ▸ Exit ▸ Restart KOReader* (on a Boox or
   Android, *☰ ▸ Exit*, then open KOReader again).
2. **KOReader:** open a book and tap the top of the page for its menu. On the *Tools* tab (the
   wrench), choose *Progress sync ▸ Custom sync server*. The box starts with `https://`: make it
   `http://`, then the server's address and `:17200`, and tap *OK*. Then choose *Register / Login*. The first time, pick a name and password and tap
   *Register*. The plugin uses this same account.
3. **So it syncs by itself:** on a Kobo or Kindle, on the *Settings* tab (the gear), choose
   *Network*, set *Action when Wi-Fi is off* to *Turn on* (KOReader won't let the next tick on
   until then) and tick *Restore Wi-Fi connection on resume*, so it syncs as it wakes. Then tick *Tools ▸ Progress sync ▸ Automatically keep documents in sync*.
4. **Athene:** *Settings ▸ KOReader*, turn on *Sync with KOReader*, and sign in with the same address, name and password.
   In KOReader, Athene's own menu, with *Sync now*, is on the *Tools* tab, right under *Progress sync*.

## Update it

```sh
docker pull athenereader/athene-sync
docker rm -f athene-sync
docker run -d --name athene-sync --restart unless-stopped \
  -p 17200:17200 -v athene-sync:/data athenereader/athene-sync
```

Your accounts, books and annotations stay in the `athene-sync` volume. On a NAS, pull the image
again in its Docker app and recreate the container with the same port and folder.

## What it keeps

One folder, `/data`: `accounts.json` (each account's name and a salted PBKDF2 hash of the key
KOReader sends, itself an MD5 of the password), and per account a small JSON file for the places,
one per book for its annotations, a list of books with the books themselves, and the devices
that have synced. A book is known by KOReader's fingerprint of its file.

## The API

KOReader's Progress sync, exactly: `POST /users/create`, `GET /users/auth`,
`PUT /syncs/progress`, `GET /syncs/progress/:document`, `GET /healthcheck` (each with
`Accept: application/vnd.koreader.v1+json`).

Full sync, under `/athene/v1`, signed in the same way (`x-auth-user`, `x-auth-key`). A device
names itself in `X-Athene-Device`, `X-Athene-Device-Id` and `X-Athene-Client` (percent-encoded):

| | |
|---|---|
| `GET info` | `{server, version, api, plugin, registration}`; no account needed |
| `GET annotations/:document?since=n` | `{rev, annotations}`: what changed since `n` |
| `POST annotations/:document` | `{since, annotations}` in, the same answer out: each record kept if it is newer |
| `GET annotations` | `{documents: {document: rev}}` |
| `GET books` · `POST books?filename=&title=&authors=` (the file as the body) | the books for the e-reader |
| `GET books/:id` · `GET books/:id/file` · `POST books/:id/delivered` · `DELETE books/:id` | |
| `GET devices` | the devices that have synced, newest first |
| `GET plugin` · `GET plugin/files/:path` | the plugin's files, for it to update itself |

An annotation record is a JSON object. The server reads only `id`, `updated` (milliseconds) and
`deleted`; the newer `updated` wins, a deletion keeps only those three, and deletions are kept
for a year. The clients agree on the rest: `kind` (`highlight` or `bookmark`), `page`, `pos0`,
`pos1` (KOReader's XPointers, or for a PDF its page and points), `text`, `note`, `color`,
`drawer`, `chapter`, `created`, `device`, `device_id`.

## Licence

Copyright © 2026 Aditya Sreedhar. Free software under the GNU Affero General Public License,
version 3 (see `LICENSE`). A running server offers its own source at `/source`.
