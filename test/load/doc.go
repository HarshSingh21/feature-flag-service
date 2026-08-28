// Package load is the load and throughput benchmark suite for the flag service.
//
// It exists to substantiate — or falsify — the performance claims in
// docs/03-lld.md §2 and §4, against the specification in
// docs/05-consistency-and-e2e.md §3.
//
// The package deliberately contains no production code. Everything lives in
// _test.go files; this file exists only so that `go build ./...` has a package
// to compile. Nothing here is imported by the service or the client.
//
// Run it with:
//
//	go test ./test/load/ -run TestPassCriteria -v
//	go test ./test/load/ -bench . -benchtime 3s -run '^$'
//
// See README.md in this directory for the full scenario map and the measured
// results on the reference machine.
package load
