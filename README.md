# go-imap

[![Go Reference](https://pkg.go.dev/badge/github.com/migadu/go-imap.svg)](https://pkg.go.dev/github.com/migadu/go-imap)

An [IMAP4rev2] client and server library for Go, maintained by [Migadu].

## Relationship to emersion/go-imap

This is a hard fork of [emersion/go-imap] v2. It started as a regular fork,
but has since diverged by several hundred commits and is now developed
independently:

- The module path is `github.com/migadu/go-imap`, versioned from v1.0.0. It is
  unrelated to upstream's v1 and v2 lines, and is not a drop-in
  `replace` target for `github.com/emersion/go-imap/v2`; import paths have to
  be changed.
- Development happens on the `main` branch.
- The API follows upstream v2 where the two still overlap, but compatibility
  is not a goal and neither API should be assumed to match the other.
- Upstream fixes are still picked up when they apply, with their original
  authorship.

Many thanks to Simon Ser and the go-imap contributors for the work this
library is built on. Comments that reference `emersion/go-imap#N` point at
upstream issues and pull requests.

## What is different

Compared to upstream, this fork focuses on running in production on both sides
of the connection:

- **Server extensions**: CONDSTORE and QRESYNC, SORT, ESORT and THREAD, ESEARCH,
  SEARCHRES and MULTISEARCH, ACL, METADATA, NOTIFY, ID, APPENDLIMIT and
  UNAUTHENTICATE, each exposed as an optional `imapserver.Session*` interface
  and advertised only when the backend implements it.
- **Hardening**: bounds on literals, nesting depth and buffered data, read
  timeouts, and fixes for races and panics on both client and server.
- **Interoperability**: the client tolerates a number of malformed responses
  seen from real servers, and the server is stricter about what it emits.

## Usage

To add go-imap to your project, run:

    go get github.com/migadu/go-imap

Documentation and examples for the module are available here:

- [Client docs]
- [Server docs]

`cmd/imapmemserver` is a small in-memory server useful for testing:

    go run github.com/migadu/go-imap/cmd/imapmemserver@latest -insecure-auth

## License

MIT, see [LICENSE](LICENSE).

[IMAP4rev2]: https://www.rfc-editor.org/rfc/rfc9051.html
[Migadu]: https://migadu.com
[emersion/go-imap]: https://github.com/emersion/go-imap
[Client docs]: https://pkg.go.dev/github.com/migadu/go-imap/imapclient
[Server docs]: https://pkg.go.dev/github.com/migadu/go-imap/imapserver
