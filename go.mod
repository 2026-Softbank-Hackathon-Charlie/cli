module github.com/2026-Softbank-Hackathon-Charlie/cli

go 1.26.0

require (
	github.com/2026-Softbank-Hackathon-Charlie/platform v0.0.0-20261102022506-95b9aaaf50b5
	github.com/spf13/cobra v1.10.2
	golang.org/x/mod v0.42.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)

// Development only: builds against the combined platform worktree. Remove before merging
// and require the merged platform commit.
replace github.com/2026-Softbank-Hackathon-Charlie/platform => ../p-sec
