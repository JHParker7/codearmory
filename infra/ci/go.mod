// Its own tiny module so the pruner can be unit-tested. Deliberately outside
// src/go.work and dependency-free: CI runs it with `go run ./infra/ci`, which must
// not need a module download.
module codearmory/infra/ci

go 1.25
