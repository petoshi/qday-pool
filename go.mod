module github.com/petoshi/qday-pool

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.47
	go.sia.tech/core v0.21.6
	golang.org/x/crypto v0.54.0
)

require (
	github.com/cloudflare/circl v1.6.5 // indirect
	golang.org/x/sys v0.47.0 // indirect
	lukechampine.com/frand v1.5.1 // indirect
)

replace go.sia.tech/core => ./qday/core
