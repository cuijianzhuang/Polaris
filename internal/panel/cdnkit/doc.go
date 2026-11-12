// Package cdnkit holds the Server-independent math of CDN profiles and
// speed-test reports: scheme/port selection, carrier and region
// normalisation, report validation and merging, the address-check
// verdicts, and the report selection and list-rebuild rules. It depends
// only on the store and model types, never on *panel.Server, the database
// or configuration, so it can be tested and reused without a panel. The
// database is hidden behind the ReportStore interface (store.go), which
// package panel implements with gorm; anything that talks to a node or
// serves HTTP stays in package panel.
package cdnkit
