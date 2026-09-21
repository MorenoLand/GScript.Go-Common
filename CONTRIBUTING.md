# Contributing

## Development setup

Install Go 1.25 or newer, clone the repository, and run:

    go test ./...
    go build ./...

## Changes

Keep shared code platform-neutral and independent from application-specific policy. Prefer small, reusable APIs with clear ownership and avoid adding client, server, or editor assumptions to common packages.

Keep changes focused, format Go code with gofmt, and add tests for behavior that can be verified without external services or application windows.

## Commits and pull requests

Use a short imperative commit subject. Describe the behavior changed, the platforms checked, and any remaining limitations in the pull request.

Pull requests should include passing `go test ./...` and `go build ./...` results.
