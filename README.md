# Shared Go foundation

GScript.Go-Common contains platform-independent Go libraries and utilities shared by MorenoLand projects. It is kept independent from any one client, server, editor, or game runtime so shared behavior can be tested and versioned once.

## Current libraries

The repository currently includes an MPQ reader that supports standard MPQ headers, encrypted hash and block tables, listfiles, sector offset tables, raw sectors, zlib sectors, and the standard file-key rules used by the bundled archives. Additional reusable framework and tooling code belongs here when it has more than one legitimate consumer.

## Development

    go test ./...
    go build ./...

Keep packages platform-neutral where possible, preserve stable public APIs, and add tests for behavior that can be verified without launching an application.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE).
