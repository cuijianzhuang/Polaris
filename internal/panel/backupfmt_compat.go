package panel

import "github.com/cuijianzhuang/polaris/internal/backupfmt"

// Names kept for code compiled into this package from polaris-pro, which
// still calls them unqualified. New code uses package backupfmt directly.
var (
	exportSnapshot = backupfmt.ExportSnapshot
	importAll      = backupfmt.ImportAll
)
