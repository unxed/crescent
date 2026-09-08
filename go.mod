module github.com/unxed/crescent

go 1.25.5

// goWidgets asks for purego; a `replace` inside a dependency is ignored by Go,
// so the redirect to pureffi has to be repeated here, in the main module.
replace github.com/ebitengine/purego => github.com/unxed/pureffi v0.1.16

require (
	github.com/unxed/goWidgets v0.0.0-20260908153618-ea666b9ef48c
	golang.org/x/sys v0.31.0
)

require (
	github.com/ebitengine/purego v0.9.0 // indirect
	github.com/go-webgpu/goffi v0.6.2 // indirect
)
