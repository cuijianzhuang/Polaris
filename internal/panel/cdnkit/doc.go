// Package cdnkit holds the Server-independent math of CDN profiles and
// speed-test reports: scheme/port selection, carrier and region
// normalisation, report validation and merging, and the address-check
// verdicts. It depends only on the store and model types, never on
// *panel.Server, the database or configuration, so it can be tested and
// reused without a panel. Anything that reads the database, talks to a
// node or serves HTTP stays in package panel.
package cdnkit
