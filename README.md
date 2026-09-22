# Shared Go foundation

GScript.Go-Common contains platform-independent Go libraries and utilities shared by MorenoLand projects. It is kept independent from any one client, server, editor, or game runtime so shared behavior can be tested and versioned once.

## Shared components

Packages in this repository are platform-independent Go components and utilities shared by multiple MorenoLand projects. Add a component here when it has more than one legitimate consumer; keep project-specific behavior in the owning repository.

## Development

    go test ./...
    go build ./...

Keep packages platform-neutral where possible, preserve stable public APIs, and add tests for behavior that can be verified without launching an application.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE).
