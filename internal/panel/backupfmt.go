package panel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql/driver"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Backup format 2: a logical export. Every table is written as rows of
// plain column values (gob), so an archive restores into SQLite or
// PostgreSQL alike, whichever the panel was using when it was made.
//
//	manifest.json      format, panel version, row counts
//	tables/<t>.gob     the rows, in batches
//	ca.crt, ca.key     the internal CA (ca.key sealed with the secret key)
//	secret.key         the panel's secret key, only in encrypted archives
//
// An archive can be encrypted with a passphrase (scrypt + AES-GCM, in
// chunks). Encrypted archives carry the secret key, so they restore on a new
// server with a different key: every sealed value is re-sealed with the new
// one. Format 1 archives (a SQLite file) are converted on restore.

const (
	backupFormat = 2
	encMagic     = "PLRSENC1"
	encChunk     = 64 << 10
	exportBatch  = 1000
)

var (
	errNeedPassphrase = errors.New("这是加密备份，请输入备份口令")
	errBadPassphrase  = errors.New("备份口令错误，或文件已损坏")
	sealedRe          = regexp.MustCompile(`enc:v1:[A-Za-z0-9+/]+`)
)

func init() {
	gob.Register(time.Time{})
	gob.Register([]byte(nil))
}

type backupManifest struct {
	Format  int            `json:"format"`
	Created time.Time      `json:"created"`
	Version string         `json:"version"`
	DB      string         `json:"db"`
	Tables  map[string]int `json:"tables"`
	WithKey bool           `json:"withKey"`
}

// --- rows ---

func tableSchema(db *gorm.DB, model any) (*schema.Schema, error) {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil {
		return nil, err
	}
	return stmt.Schema, nil
}

// columnValue turns a struct field into a plain value: JSON columns become
// their text, pointers their target (or nil), numbers int64 / float64.
func columnValue(f *schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if vr, ok := v.(driver.Valuer); ok {
		dv, err := vr.Value()
		if err != nil {
			return nil, err
		}
		return columnValue(f, dv)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr:
		if rv.IsNil() {
			return nil, nil
		}
		return columnValue(f, rv.Elem().Interface())
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			if f.DataType == schema.Bytes {
				return append([]byte(nil), rv.Bytes()...), nil
			}
			return string(rv.Bytes()), nil // e.g. json.RawMessage in a text column
		}
	}
	if t, ok := v.(time.Time); ok {
		return t, nil
	}
	return nil, fmt.Errorf("%s: unsupported column type %T", f.DBName, v)
}

// exportTable streams a table as batches of column maps.
func exportTable(db *gorm.DB, model any, emit func([]map[string]any) error) (int, error) {
	sch, err := tableSchema(db, model)
	if err != nil {
		return 0, err
	}
	typ := reflect.TypeOf(model).Elem()
	order := strings.Join(sch.PrimaryFieldDBNames, ", ")
	n := 0
	for off := 0; ; off += exportBatch {
		slice := reflect.New(reflect.SliceOf(typ))
		q := db.Model(model).Limit(exportBatch).Offset(off)
		if order != "" {
			q = q.Order(order)
		}
		if err := q.Find(slice.Interface()).Error; err != nil {
			return n, err
		}
		sv := slice.Elem()
		rows := make([]map[string]any, 0, sv.Len())
		for i := 0; i < sv.Len(); i++ {
			row := map[string]any{}
			for _, f := range sch.Fields {
				if f.DBName == "" {
					continue
				}
				v, _ := f.ValueOf(context.Background(), sv.Index(i))
				cv, err := columnValue(f, v)
				if err != nil {
					return n, fmt.Errorf("%s.%w", sch.Table, err)
				}
				row[f.DBName] = cv
			}
			rows = append(rows, row)
		}
		if len(rows) > 0 {
			if err := emit(rows); err != nil {
				return n, err
			}
		}
		n += len(rows)
		if sv.Len() < exportBatch {
			return n, nil
		}
	}
}

// exportAll dumps every table: name -> gob stream of row batches.
func exportAll(db *gorm.DB) (map[string][]byte, map[string]int, error) {
	files, counts := map[string][]byte{}, map[string]int{}
	for _, m := range store.AllModels() {
		sch, err := tableSchema(db, m)
		if err != nil {
			return nil, nil, err
		}
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		n, err := exportTable(db, m, func(rows []map[string]any) error { return enc.Encode(rows) })
		if err != nil {
			return nil, nil, fmt.Errorf("导出 %s 失败: %w", sch.Table, err)
		}
		files[sch.Table], counts[sch.Table] = buf.Bytes(), n
	}
	return files, counts, nil
}

// importAll replaces every table's rows with the archive's, re-sealing
// secrets with reseal. Columns the current schema lacks are dropped; tables
// the archive lacks end up empty.
func importAll(tx *gorm.DB, tables map[string][]byte, reseal func(string) string) error {
	for _, m := range store.AllModels() {
		sch, err := tableSchema(tx, m)
		if err != nil {
			return err
		}
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(m).Error; err != nil {
			return fmt.Errorf("清空 %s 失败: %w", sch.Table, err)
		}
		data, ok := tables[sch.Table]
		if !ok || len(data) == 0 {
			continue
		}
		cols := map[string]bool{}
		for _, f := range sch.Fields {
			if f.DBName != "" {
				cols[f.DBName] = true
			}
		}
		dec := gob.NewDecoder(bytes.NewReader(data))
		for {
			var rows []map[string]any
			if err := dec.Decode(&rows); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("读取 %s 失败: %w", sch.Table, err)
			}
			for _, r := range rows {
				for k, v := range r {
					switch x := v.(type) {
					case string:
						r[k] = reseal(x)
					case []byte:
						r[k] = []byte(reseal(string(x)))
					}
					if !cols[k] {
						delete(r, k)
					}
				}
			}
			for i := 0; i < len(rows); i += 100 {
				batch := rows[i:min(i+100, len(rows))]
				if err := tx.Model(m).Create(&batch).Error; err != nil {
					return fmt.Errorf("写入 %s 失败: %w", sch.Table, err)
				}
			}
		}
		// PostgreSQL: explicit ids leave the serial sequence behind.
		if tx.Dialector.Name() == "postgres" {
			for _, f := range sch.Fields {
				if f.AutoIncrement && f.DBName != "" {
					tx.Exec(fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
						sch.Table, f.DBName, f.DBName, sch.Table))
				}
			}
		}
	}
	return nil
}

// resealer re-encrypts values sealed with from so that to opens them.
func resealer(from, to *secret.Box) func(string) string {
	if from == to {
		return func(s string) string { return s }
	}
	return func(s string) string {
		if !strings.Contains(s, "enc:v1:") {
			return s
		}
		return sealedRe.ReplaceAllStringFunc(s, func(m string) string {
			if pt, err := from.Open(m); err == nil {
				return to.Seal(pt)
			}
			return m
		})
	}
}

// --- encryption ---

func backupKey(pass string, salt []byte) (cipher.AEAD, error) {
	k, err := scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

type encWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix [4]byte
	ctr    uint64
	buf    []byte
}

// encryptWriter wraps w: magic, salt, nonce prefix, then sealed chunks
// (length-prefixed; the last one is marked in its additional data).
func encryptWriter(w io.Writer, pass string) (io.WriteCloser, error) {
	salt := make([]byte, 16)
	e := &encWriter{w: w}
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(e.prefix[:]); err != nil {
		return nil, err
	}
	aead, err := backupKey(pass, salt)
	if err != nil {
		return nil, err
	}
	e.aead = aead
	if _, err := w.Write(append(append([]byte(encMagic), salt...), e.prefix[:]...)); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *encWriter) nonce() []byte {
	n := make([]byte, 12)
	copy(n, e.prefix[:])
	binary.BigEndian.PutUint64(n[4:], e.ctr)
	return n
}

func (e *encWriter) chunk(p []byte, final bool) error {
	aad := []byte{0}
	if final {
		aad[0] = 1
	}
	ct := e.aead.Seal(nil, e.nonce(), p, aad)
	e.ctr++
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := e.w.Write(l[:]); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	return err
}

func (e *encWriter) Write(p []byte) (int, error) {
	e.buf = append(e.buf, p...)
	for len(e.buf) > encChunk {
		if err := e.chunk(e.buf[:encChunk], false); err != nil {
			return 0, err
		}
		e.buf = e.buf[encChunk:]
	}
	return len(p), nil
}

func (e *encWriter) Close() error { return e.chunk(e.buf, true) }

type decReader struct {
	r    io.Reader
	enc  encWriter // nonce state
	buf  []byte
	done bool
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.done {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(d.r, l[:]); err != nil {
			return 0, errors.New("备份文件不完整")
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > encChunk+64 {
			return 0, errBadPassphrase
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			return 0, errors.New("备份文件不完整")
		}
		nonce := d.enc.nonce()
		pt, err := d.enc.aead.Open(nil, nonce, ct, []byte{0})
		if err != nil {
			if pt, err = d.enc.aead.Open(nil, nonce, ct, []byte{1}); err != nil {
				return 0, errBadPassphrase
			}
			d.done = true
		}
		d.enc.ctr++
		d.buf = pt
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

// decryptReader undoes encryptWriter; r must start at the magic.
func decryptReader(r io.Reader, pass string) (io.Reader, error) {
	head := make([]byte, len(encMagic)+16+4)
	if _, err := io.ReadFull(r, head); err != nil || string(head[:len(encMagic)]) != encMagic {
		return nil, errors.New("不是加密备份")
	}
	if pass == "" {
		return nil, errNeedPassphrase
	}
	aead, err := backupKey(pass, head[len(encMagic):len(encMagic)+16])
	if err != nil {
		return nil, err
	}
	d := &decReader{r: r}
	d.enc.aead = aead
	copy(d.enc.prefix[:], head[len(encMagic)+16:])
	return d, nil
}

// --- archives ---

type backupArchive struct {
	Manifest backupManifest
	Tables   map[string][]byte
	CACert   []byte
	CAKey    []byte
	Key      string // secret key of the source panel (encrypted archives)
}

// writeArchive writes a format-2 archive of st (and the CA in dataDir) to w,
// encrypted when pass is set.
func writeArchive(w io.Writer, st *store.Store, dataDir, secretKey, pass string) error {
	tables, counts, err := exportAll(st.DB)
	if err != nil {
		return err
	}
	out := io.Writer(w)
	var ew io.WriteCloser
	if pass != "" {
		if ew, err = encryptWriter(w, pass); err != nil {
			return err
		}
		out = ew
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	man, _ := json.MarshalIndent(backupManifest{Format: backupFormat, Created: time.Now(), Version: Version,
		DB: st.DB.Dialector.Name(), Tables: counts, WithKey: pass != ""}, "", "  ")
	if err := add("manifest.json", man); err != nil {
		return err
	}
	for name, b := range tables {
		if err := add("tables/"+name+".gob", b); err != nil {
			return err
		}
	}
	for _, n := range []string{"ca.crt", "ca.key"} {
		if b, err := os.ReadFile(filepath.Join(dataDir, n)); err == nil {
			if err := add(n, b); err != nil {
				return err
			}
		}
	}
	if pass != "" {
		if err := add("secret.key", []byte(secretKey)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if ew != nil {
		return ew.Close()
	}
	return nil
}

// readArchive reads a backup (encrypted or not, format 1 or 2) into memory.
func readArchive(r io.Reader, pass string, box *secret.Box) (*backupArchive, error) {
	head := make([]byte, 16)
	n, _ := io.ReadFull(r, head)
	r = io.MultiReader(bytes.NewReader(head[:n]), io.LimitReader(r, maxUpload))
	if n >= len(encMagic) && string(head[:len(encMagic)]) == encMagic {
		dr, err := decryptReader(r, pass)
		if err != nil {
			return nil, err
		}
		r = dr
		head = make([]byte, 16)
		n, _ = io.ReadFull(r, head)
		r = io.MultiReader(bytes.NewReader(head[:n]), r)
	}
	files := map[string][]byte{}
	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("解压失败: %w", err)
		}
		tr := tar.NewReader(gz)
		total := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("解压失败: %w", err)
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxUpload))
			if err != nil {
				return nil, fmt.Errorf("解压失败: %w", err)
			}
			if total += len(b); total > maxUpload {
				return nil, errors.New("备份过大")
			}
			files[h.Name] = b
		}
	case n == 16 && string(head) == "SQLite format 3\x00":
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		files["polaris.db"] = b
	default:
		return nil, errors.New("不是 Polaris 备份文件")
	}
	arc := &backupArchive{CACert: files["ca.crt"], CAKey: files["ca.key"], Key: string(files["secret.key"]), Tables: map[string][]byte{}}
	if (arc.CACert == nil) != (arc.CAKey == nil) {
		return nil, errors.New("备份中的 CA 证书不完整")
	}
	if db, ok := files["polaris.db"]; ok { // format 1: convert the SQLite file
		tables, counts, err := convertLegacyDB(db, box)
		if err != nil {
			return nil, err
		}
		arc.Tables, arc.Manifest = tables, backupManifest{Format: 1, DB: "sqlite", Tables: counts}
		return arc, nil
	}
	man, ok := files["manifest.json"]
	if !ok {
		return nil, errors.New("备份中没有数据")
	}
	if err := json.Unmarshal(man, &arc.Manifest); err != nil {
		return nil, fmt.Errorf("备份清单损坏: %w", err)
	}
	if arc.Manifest.Format > backupFormat {
		return nil, fmt.Errorf("备份来自更新版本的面板（格式 %d），请先升级面板", arc.Manifest.Format)
	}
	for name, b := range files {
		if t, ok := strings.CutPrefix(name, "tables/"); ok {
			arc.Tables[strings.TrimSuffix(t, ".gob")] = b
		}
	}
	return arc, nil
}

func convertLegacyDB(db []byte, box *secret.Box) (map[string][]byte, map[string]int, error) {
	dir, err := os.MkdirTemp("", "polaris-restore-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "polaris.db")
	if err := os.WriteFile(path, db, 0o600); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(path, box, false)
	if err != nil {
		return nil, nil, fmt.Errorf("数据库无法打开: %w", err)
	}
	defer st.Close()
	var check string
	if st.DB.Raw("PRAGMA integrity_check").Scan(&check); check != "ok" {
		return nil, nil, fmt.Errorf("数据库已损坏: %s", check)
	}
	return exportAll(st.DB)
}

// sourceBox is the box that opens the archive's sealed values: its own key
// when it carries one, else this panel's — checked against the CA key.
func (arc *backupArchive) sourceBox(box *secret.Box) (*secret.Box, error) {
	src := box
	if arc.Key != "" {
		b, err := secret.New(arc.Key)
		if err != nil {
			return nil, err
		}
		src = b
	}
	if arc.CAKey != nil {
		if _, err := src.Open(strings.TrimSpace(string(arc.CAKey))); err != nil && strings.HasPrefix(strings.TrimSpace(string(arc.CAKey)), "enc:v1:") {
			return nil, errors.New("无法解密备份：它来自使用不同 secret_key 的面板。请在原面板设置备份口令后重新备份（加密备份会附带密钥，可恢复到任何服务器）")
		}
	}
	return src, nil
}

// validate checks an archive can be restored into this panel.
func (arc *backupArchive) validate(box *secret.Box) error {
	if _, err := arc.sourceBox(box); err != nil {
		return err
	}
	if len(arc.Tables["admins"]) == 0 {
		return errors.New("备份中没有管理员账号")
	}
	return nil
}

// restoreArchive replaces the database (and CA files in dataDir) with the
// archive's, all or nothing for the database.
func restoreArchive(st *store.Store, dataDir string, arc *backupArchive, box *secret.Box) error {
	src, err := arc.sourceBox(box)
	if err != nil {
		return err
	}
	reseal := resealer(src, box)
	if err := st.DB.Transaction(func(tx *gorm.DB) error { return importAll(tx, arc.Tables, reseal) }); err != nil {
		return err
	}
	if arc.CACert != nil {
		if err := os.WriteFile(filepath.Join(dataDir, "ca.crt"), arc.CACert, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dataDir, "ca.key"), []byte(reseal(string(arc.CAKey))), 0o600); err != nil {
			return err
		}
	}
	return nil
}
