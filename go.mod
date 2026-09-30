module github.com/unxed/crescent

go 1.26.0

// goWidgets asks for purego; a `replace` inside a dependency is ignored by Go,
// so the redirect to pureffi has to be repeated here, in the main module.
replace github.com/ebitengine/purego => github.com/unxed/pureffi v0.1.19

require (
	github.com/unxed/goWidgets v0.0.0-20260930030315-ad74551ce667
	github.com/unxed/winkeys v0.1.1
	golang.org/x/sys v0.31.0
)

require (
	github.com/ebitengine/purego v0.9.0 // indirect
	github.com/go-webgpu/goffi v0.6.2 // indirect
	github.com/unxed/kiwi-go v0.1.0 // indirect
)
