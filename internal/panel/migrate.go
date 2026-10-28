package panel

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// MigrateResult reports what Migrate copied.
type MigrateResult struct {
	Tables []MigrateTable
	Rows   int
	// Truncated lists "table.column" values that were cut to fit the
	// target's column size (SQLite never enforced sizes).
	Truncated []string
}

// MigrateTable is one table's row count in the source and the target.
type MigrateTable struct {
	Name           string
	Source, Target int64
}

// Migrate copies every table from the configured database to targetDSN
// (SQLite -> PostgreSQL, or back) in one transaction, then checks the row
// counts. The target must be empty unless replace is set. The panel must be
// stopped first; files in the data directory (CA, certificates) stay where
// they are, and secrets stay sealed with the same secret key.
func Migrate(cfg *Config, targetDSN string, replace bool) (*MigrateResult, error) {
	targetDSN = strings.TrimSpace(targetDSN)
	if targetDSN == "" {
		return nil, errors.New("目标数据库为空")
	}
	if targetDSN == cfg.DB.DSN {
		return nil, errors.New("目标与当前数据库相同")
	}
	if !store.IsPostgresDSN(cfg.DB.DSN) {
		path, _, _ := strings.Cut(cfg.DB.DSN, "?")
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("当前数据库 %s 不存在（数据目录或 POLARIS_DB_DSN 是否正确？）", path)
		}
	}
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	src, err := store.Open(cfg.DB.DSN, box, false)
	if err != nil {
		return nil, fmt.Errorf("打开当前数据库失败: %w", err)
	}
	defer src.Close()
	dst, err := store.Open(targetDSN, box, false) // creates the tables
	if err != nil {
		return nil, fmt.Errorf("打开目标数据库失败: %w", err)
	}
	defer dst.Close()

	if !replace {
		for _, m := range store.AllModels() {
			var n int64
			if err := dst.DB.Model(m).Count(&n).Error; err != nil {
				return nil, err
			}
			if n > 0 {
				sch, _ := backupfmt.TableSchema(dst.DB, m)
				return nil, fmt.Errorf("目标数据库已有数据（%s 表 %d 行）；确认要覆盖请加 -replace", sch.Table, n)
			}
		}
	}

	res := &MigrateResult{}
	err = dst.DB.Transaction(func(tx *gorm.DB) error {
		for _, m := range store.AllModels() {
			sch, err := backupfmt.TableSchema(tx, m)
			if err != nil {
				return err
			}
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(m).Error; err != nil {
				return fmt.Errorf("清空 %s 失败: %w", sch.Table, err)
			}
			n, err := backupfmt.ExportTable(src.DB, m, func(rows []map[string]any) error {
				cut, err := backupfmt.InsertRows(tx, m, sch, rows)
				res.Truncated = appendNew(res.Truncated, cut...)
				return err
			})
			if err != nil {
				return fmt.Errorf("复制 %s 失败: %w", sch.Table, err)
			}
			res.Rows += n
			if err := backupfmt.ResetSequences(tx, sch); err != nil {
				return err
			}
		}
		// Verify inside the transaction: a mismatch leaves the target as it was.
		for _, m := range store.AllModels() {
			sch, _ := backupfmt.TableSchema(tx, m)
			t := MigrateTable{Name: sch.Table}
			if err := src.DB.Model(m).Count(&t.Source).Error; err != nil {
				return err
			}
			if err := tx.Model(m).Count(&t.Target).Error; err != nil {
				return err
			}
			if t.Source != t.Target {
				return fmt.Errorf("%s 行数不一致：源 %d，目标 %d（迁移期间面板仍在运行？）", t.Name, t.Source, t.Target)
			}
			res.Tables = append(res.Tables, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func appendNew(list []string, xs ...string) []string {
	for _, x := range xs {
		found := false
		for _, y := range list {
			if y == x {
				found = true
				break
			}
		}
		if !found {
			list = append(list, x)
		}
	}
	return list
}
