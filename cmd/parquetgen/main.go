package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"text/template"

	"github.com/parsyl/parquet/internal/parse"
)

var (
	typ    = flag.String("type", "", "type name")
	pkg    = flag.String("package", "", "package name")
	imp    = flag.String("import", "", "the type's import statement (only if it doesn't live in 'package')")
	pth    = flag.String("input", "", "path to the go file that defines -type")
	ignore = flag.Bool("ignore", true, "ignore unsupported fields in -type")
)

func main() {
	flag.Parse()

	result, err := parse.Fields(*typ, *pth)
	if err != nil {
		log.Fatal(err)
	}

	i := input{
		Package: *pkg,
		Type:    *typ,
		Import:  getImport(*imp),
		Fields:  result.Fields,
	}

	for _, err := range result.Errors {
		log.Println(err)
	}

	if len(result.Errors) > 0 && !*ignore {
		log.Fatal("not generating parquet.go (-ignore set to false)")
	}

	tmpl, err := template.New("output").Parse(tpl)
	if err != nil {
		log.Fatal(err)
	}

	f, err := os.Create("parquet.go")
	if err != nil {
		log.Fatal(err)
	}

	err = tmpl.Execute(f, i)
	if err != nil {
		log.Fatal(err)
	}

	f.Close()
}

func getImport(i string) string {
	if i == "" {
		return ""
	}
	return fmt.Sprintf(`"%s"`, i)
}

type input struct {
	Package string
	Type    string
	Import  string
	Fields  []string
}

var tpl = `package {{.Package}}

// This code is generated by github.com/parsyl/parquet.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/golang/snappy"
	"github.com/parsyl/parquet"
	{{.Import}}
)

// ParquetWriter reprents a row group
type ParquetWriter struct {
	fields []Field

	len int

	// child points to the next page
	child *ParquetWriter

	// max is the number of Record items that can get written before
	// a new set of column chunks is written
	max int

	meta *parquet.Metadata
	w    *WriteCounter
}

func Fields() []Field {
	return []Field{ {{range .Fields}}
		{{.}}{{end}}
	}
}

func NewParquetWriter(w io.Writer, opts ...func(*ParquetWriter) error) (*ParquetWriter, error) {
	return newParquetWriter(w, append(opts, begin)...)
}

func newParquetWriter(w io.Writer, opts ...func(*ParquetWriter) error) (*ParquetWriter, error) {
	p := &ParquetWriter{
		max:    1000,
		w:      &WriteCounter{w: w},
		fields: Fields(),
	}

	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}

	if p.meta == nil {
		ff := Fields()
		schema := make([]parquet.Field, len(ff))
		for i, f := range ff {
			schema[i] = f.Schema()
		}
		p.meta = parquet.New(schema...)
	}

	return p, nil
}

// MaxPageSize is the maximum number of rows in each row groups' page.
func MaxPageSize(m int) func(*ParquetWriter) error {
	return func(p *ParquetWriter) error {
		p.max = m
		return nil
	}
}

func begin(p *ParquetWriter) error {
	_, err := p.w.Write([]byte("PAR1"))
	return err
}

func withMeta(m *parquet.Metadata) func(*ParquetWriter) error {
	return func(p *ParquetWriter) error {
		p.meta = m
		return nil
	}
}

func (p *ParquetWriter) Write() error {
	for i, f := range p.fields {
		if err := f.Write(p.w, p.meta); err != nil {
			return err
		}

		for child := p.child; child != nil; child = child.child {
			if err := child.fields[i].Write(p.w, p.meta); err != nil {
				return err
			}
		}
	}

	p.fields = Fields()
	p.child = nil
	p.len = 0

	schema := make([]parquet.Field, len(p.fields))
	for i, f := range p.fields {
		schema[i] = f.Schema()
	}
	p.meta.StartRowGroup(schema...)
	return nil
}

func (p *ParquetWriter) Close() error {
	if err := p.meta.Footer(p.w); err != nil {
		return err
	}

	_, err := p.w.Write([]byte("PAR1"))
	return err
}

func (p *ParquetWriter) Add(rec {{.Type}}) {
	if p.len == p.max {
		if p.child == nil {
			// an error can't happen here
			p.child, _ = newParquetWriter(p.w, MaxPageSize(p.max), withMeta(p.meta))
		}

		p.child.Add(rec)
		return
	}

	for _, f := range p.fields {
		f.Add(rec)
	}

	p.len++
}

type Field interface {
	Add(r {{.Type}})
	Write(w io.Writer, meta *parquet.Metadata) error
	Schema() parquet.Field
	Scan(r *{{.Type}})
	Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error
	Name() string
}

type RequiredField struct {
	col string
}

func (f *RequiredField) doWrite(w io.Writer, meta *parquet.Metadata, vals []byte, count int) error {
	compressed := snappy.Encode(nil, vals)
	if err := meta.WritePageHeader(w, f.col, len(vals), len(compressed), count); err != nil {
		return err
	}

	_, err := w.Write(compressed)
	return err
}

func (f *RequiredField) doRead(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) (io.Reader, error) {
	var nRead int
	var out []byte

	for nRead < pos.N {
		ph, err := meta.PageHeader(r)
		if err != nil {
			return nil, err
		}

		compressed := make([]byte, ph.CompressedPageSize)
		if _, err := r.Read(compressed); err != nil {
			return nil, err
		}

		data, err := snappy.Decode(nil, compressed)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		nRead += int(ph.DataPageHeader.NumValues)
	}

	return bytes.NewBuffer(out), nil
}

func (f *RequiredField) Name() string {
	return f.col
}

type OptionalField struct {
	defs []int64
	col  string
}

func (f *OptionalField) nVals() int {
	var out int
	for _, d := range f.defs {
		if d == 1 {
			out++
		}
	}
	return out
}

func (f *OptionalField) doWrite(w io.Writer, meta *parquet.Metadata, vals []byte, count int) error {
	buf := bytes.Buffer{}
	wc := &WriteCounter{w: &buf}

	err := parquet.WriteLevels(wc, f.defs)
	if err != nil {
		return err
	}

	if _, err := wc.Write(vals); err != nil {
		return err
	}

	compressed := snappy.Encode(nil, buf.Bytes())
	if err := meta.WritePageHeader(w, f.col, int(wc.n), len(compressed), len(f.defs)); err != nil {
		return err
	}

	_, err = w.Write(compressed)
	return err
}

func (f *OptionalField) doRead(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) (io.Reader, error) {
	var nRead int
	var out []byte

	for nRead < pos.N {
		ph, err := meta.PageHeader(r)
		if err != nil {
			return nil, err
		}

		compressed := make([]byte, ph.CompressedPageSize)
		if _, err := r.Read(compressed); err != nil {
			return nil, err
		}

		uncompressed, err := snappy.Decode(nil, compressed)
		if err != nil {
			return nil, err
		}

		defs, l, err := parquet.ReadLevels(bytes.NewBuffer(uncompressed))

		if err != nil {
			return nil, err
		}
		f.defs = append(f.defs, defs...)
		out = append(out, uncompressed[l:]...)
		nRead += int(ph.DataPageHeader.NumValues)
	}

	return bytes.NewBuffer(out), nil
}

func (f *OptionalField) Name() string {
	return f.col
}

type Uint32Field struct {
	vals []uint32
	RequiredField
	val  func(r {{.Type}}) uint32
	read func(r *{{.Type}}, v uint32)
}

func NewUint32Field(val func(r {{.Type}}) uint32, read func(r *{{.Type}}, v uint32), col string) *Uint32Field {
	return &Uint32Field{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *Uint32Field) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Uint32Type, RepetitionType: parquet.RepetitionRequired}
}

func (f *Uint32Field) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}
	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *Uint32Field) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Uint32Field) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]uint32, int(pos.N))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Uint32Field) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

type Uint32OptionalField struct {
	OptionalField
	vals []uint32
	read func(r *{{.Type}}, v *uint32)
	val  func(r {{.Type}}) *uint32
}

func NewUint32OptionalField(val func(r {{.Type}}) *uint32, read func(r *{{.Type}}, v *uint32), col string) *Uint32OptionalField {
	return &Uint32OptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *Uint32OptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Uint32Type, RepetitionType: parquet.RepetitionOptional}
}

func (f *Uint32OptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Uint32OptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]uint32, f.nVals()-len(f.vals))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Uint32OptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

func (f *Uint32OptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *uint32
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

type Int32Field struct {
	vals []int32
	RequiredField
	val  func(r {{.Type}}) int32
	read func(r *{{.Type}}, v int32)
}

func NewInt32Field(val func(r {{.Type}}) int32, read func(r *{{.Type}}, v int32), col string) *Int32Field {
	return &Int32Field{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *Int32Field) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Int32Type, RepetitionType: parquet.RepetitionRequired}
}

func (f *Int32Field) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, f.vals); err != nil {
		return err
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Int32Field) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]int32, int(pos.N))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Int32Field) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

func (f *Int32Field) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

type Int32OptionalField struct {
	vals []int32
	OptionalField
	val  func(r {{.Type}}) *int32
	read func(r *{{.Type}}, v *int32)
}

func NewInt32OptionalField(val func(r {{.Type}}) *int32, read func(r *{{.Type}}, v *int32), col string) *Int32OptionalField {
	return &Int32OptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *Int32OptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Int32Type, RepetitionType: parquet.RepetitionOptional}
}

func (f *Int32OptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *int32
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *Int32OptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Int32OptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]int32, f.nVals()-len(f.vals))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Int32OptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

type Int64Field struct {
	vals []int64
	RequiredField
	val  func(r {{.Type}}) int64
	read func(r *{{.Type}}, v int64)
}

func NewInt64Field(val func(r {{.Type}}) int64, read func(r *{{.Type}}, v int64), col string) *Int64Field {
	return &Int64Field{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *Int64Field) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Int64Type, RepetitionType: parquet.RepetitionRequired}
}

func (f *Int64Field) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *Int64Field) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Int64Field) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]int64, int(pos.N))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Int64Field) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

type Int64OptionalField struct {
	vals []int64
	OptionalField
	val  func(r {{.Type}}) *int64
	read func(r *{{.Type}}, v *int64)
}

func NewInt64OptionalField(val func(r {{.Type}}) *int64, read func(r *{{.Type}}, v *int64), col string) *Int64OptionalField {
	return &Int64OptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *Int64OptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Int64Type, RepetitionType: parquet.RepetitionOptional}
}

func (f *Int64OptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Int64OptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]int64, int(f.nVals()-len(f.vals)))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Int64OptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *int64
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *Int64OptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

type Uint64Field struct {
	vals []uint64
	RequiredField
	val  func(r {{.Type}}) uint64
	read func(r *{{.Type}}, v uint64)
}

func NewUint64Field(val func(r {{.Type}}) uint64, read func(r *{{.Type}}, v uint64), col string) *Uint64Field {
	return &Uint64Field{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *Uint64Field) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Uint64Type, RepetitionType: parquet.RepetitionRequired}
}

func (f *Uint64Field) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Uint64Field) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]uint64, int(pos.N))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Uint64Field) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *Uint64Field) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

type Uint64OptionalField struct {
	vals []uint64
	OptionalField
	val  func(r {{.Type}}) *uint64
	read func(r *{{.Type}}, v *uint64)
}

func NewUint64OptionalField(val func(r {{.Type}}) *uint64, read func(r *{{.Type}}, v *uint64), col string) *Uint64OptionalField {
	return &Uint64OptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *Uint64OptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Uint64Type, RepetitionType: parquet.RepetitionOptional}
}

func (f *Uint64OptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Uint64OptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]uint64, int(f.nVals()-len(f.vals)))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Uint64OptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *uint64
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *Uint64OptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

type Float32Field struct {
	vals []float32
	RequiredField
	val  func(r {{.Type}}) float32
	read func(r *{{.Type}}, v float32)
}

func NewFloat32Field(val func(r {{.Type}}) float32, read func(r *{{.Type}}, v float32), col string) *Float32Field {
	return &Float32Field{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *Float32Field) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Float32Type, RepetitionType: parquet.RepetitionRequired}
}

func (f *Float32Field) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Float32Field) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]float32, int(pos.N))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Float32Field) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *Float32Field) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

type Float32OptionalField struct {
	vals []float32
	OptionalField
	val  func(r {{.Type}}) *float32
	read func(r *{{.Type}}, v *float32)
}

func NewFloat32OptionalField(val func(r {{.Type}}) *float32, read func(r *{{.Type}}, v *float32), col string) *Float32OptionalField {
	return &Float32OptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *Float32OptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.Float32Type, RepetitionType: parquet.RepetitionOptional}
}

func (f *Float32OptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	var buf bytes.Buffer
	for _, v := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *Float32OptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v := make([]float32, int(f.nVals()-len(f.vals)))
	err = binary.Read(rr, binary.LittleEndian, &v)
	f.vals = append(f.vals, v...)
	return err
}

func (f *Float32OptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *float32
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *Float32OptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)

		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

type BoolField struct {
	RequiredField
	vals []bool
	val  func(r {{.Type}}) bool
	read func(r *{{.Type}}, v bool)
}

func NewBoolField(val func(r {{.Type}}) bool, read func(r *{{.Type}}, v bool), col string) *BoolField {
	return &BoolField{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *BoolField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.BoolType, RepetitionType: parquet.RepetitionRequired}
}

func (f *BoolField) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *BoolField) Name() string {
	return f.col
}

func (f *BoolField) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

func (f *BoolField) Write(w io.Writer, meta *parquet.Metadata) error {
	ln := len(f.vals)
	byteNum := (ln + 7) / 8
	rawBuf := make([]byte, byteNum)

	for i := 0; i < ln; i++ {
		if f.vals[i] {
			rawBuf[i/8] = rawBuf[i/8] | (1 << uint32(i%8))
		}
	}

	return f.doWrite(w, meta, rawBuf, len(f.vals))
}

func (f *BoolField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	f.vals, err = parquet.GetBools(rr, int(pos.N))
	return err
}

type BoolOptionalField struct {
	OptionalField
	vals []bool
	val  func(r {{.Type}}) *bool
	read func(r *{{.Type}}, v *bool)
}

func NewBoolOptionalField(val func(r {{.Type}}) *bool, read func(r *{{.Type}}, v *bool), col string) *BoolOptionalField {
	return &BoolOptionalField{
		val:           val,
		read:          read,
		OptionalField: OptionalField{col: col},
	}
}

func (f *BoolOptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.BoolType, RepetitionType: parquet.RepetitionOptional}
}

func (f *BoolOptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	v, err := parquet.GetBools(rr, f.nVals()-len(f.vals))
	f.vals = append(f.vals, v...)
	return err
}

func (f *BoolOptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *bool
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *BoolOptionalField) Name() string {
	return f.col
}

func (f *BoolOptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

func (f *BoolOptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	ln := len(f.vals)
	byteNum := (ln + 7) / 8
	rawBuf := make([]byte, byteNum)

	for i := 0; i < ln; i++ {
		if f.vals[i] {
			rawBuf[i/8] = rawBuf[i/8] | (1 << uint32(i%8))
		}
	}

	return f.doWrite(w, meta, rawBuf, len(f.vals))
}

type StringField struct {
	RequiredField
	vals []string
	val  func(r {{.Type}}) string
	read func(r *{{.Type}}, v string)
}

func NewStringField(val func(r {{.Type}}) string, read func(r *{{.Type}}, v string), col string) *StringField {
	return &StringField{
		val:           val,
		read:          read,
		RequiredField: RequiredField{col: col},
	}
}

func (f *StringField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.StringType, RepetitionType: parquet.RepetitionRequired}
}

func (f *StringField) Scan(r *{{.Type}}) {
	if len(f.vals) == 0 {
		return
	}

	v := f.vals[0]
	f.vals = f.vals[1:]
	f.read(r, v)
}

func (f *StringField) Name() string {
	return f.col
}

func (f *StringField) Add(r {{.Type}}) {
	f.vals = append(f.vals, f.val(r))
}

func (f *StringField) Write(w io.Writer, meta *parquet.Metadata) error {
	buf := bytes.Buffer{}

	for _, s := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, int32(len(s))); err != nil {
			return err
		}
		buf.Write([]byte(s))
	}

	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *StringField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	for j := 0; j < pos.N; j++ {
		var x int32
		if err := binary.Read(rr, binary.LittleEndian, &x); err != nil {
			return err
		}
		s := make([]byte, x)
		if _, err := rr.Read(s); err != nil {
			return err
		}

		f.vals = append(f.vals, string(s))
	}
	return nil
}

type StringOptionalField struct {
	OptionalField
	vals []string
	val  func(r {{.Type}}) *string
	read func(r *{{.Type}}, v *string)
}

func NewStringOptionalField(val func(r {{.Type}}) *string, read func(r *{{.Type}}, v *string), col string) *StringOptionalField {
	return &StringOptionalField{
		val:  val,
		read: read,
		OptionalField: OptionalField{
			col: col,
		},
	}
}

func (f *StringOptionalField) Schema() parquet.Field {
	return parquet.Field{Name: f.col, Type: parquet.StringType, RepetitionType: parquet.RepetitionOptional}
}

func (f *StringOptionalField) Scan(r *{{.Type}}) {
	if len(f.defs) == 0 {
		return
	}

	var val *string
	if f.defs[0] == 1 {
		v := f.vals[0]
		f.vals = f.vals[1:]
		val = &v
	}
	f.defs = f.defs[1:]
	f.read(r, val)
}

func (f *StringOptionalField) Name() string {
	return f.col
}

func (f *StringOptionalField) Add(r {{.Type}}) {
	v := f.val(r)
	if v != nil {
		f.vals = append(f.vals, *v)
		f.defs = append(f.defs, 1)
	} else {
		f.defs = append(f.defs, 0)
	}
}

func (f *StringOptionalField) Write(w io.Writer, meta *parquet.Metadata) error {
	buf := bytes.Buffer{}

	for _, s := range f.vals {
		if err := binary.Write(&buf, binary.LittleEndian, int32(len(s))); err != nil {
			return err
		}
		buf.Write([]byte(s))
	}

	return f.doWrite(w, meta, buf.Bytes(), len(f.vals))
}

func (f *StringOptionalField) Read(r io.ReadSeeker, meta *parquet.Metadata, pos parquet.Position) error {
	start := len(f.defs)
	rr, err := f.doRead(r, meta, pos)
	if err != nil {
		return err
	}

	for j := 0; j < pos.N; j++ {
		if f.defs[start+j] == 0 {
			continue
		}

		var x int32
		if err := binary.Read(rr, binary.LittleEndian, &x); err != nil {
			return err
		}
		s := make([]byte, x)
		if _, err := rr.Read(s); err != nil {
			return err
		}

		f.vals = append(f.vals, string(s))
	}
	return nil
}

type WriteCounter struct {
	n int64
	w io.Writer
}

func (w *WriteCounter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

type ReadCounter struct {
	n int64
	r io.ReadSeeker
}

func (r *ReadCounter) Seek(o int64, w int) (int64, error) {
	return r.r.Seek(o, w)
}

func (r *ReadCounter) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func getFields(ff []Field) map[string]Field {
	m := make(map[string]Field, len(ff))
	for _, f := range ff {
		m[f.Name()] = f
	}
	return m
}

func NewParquetReader(r io.ReadSeeker, opts ...func(*ParquetReader)) (*ParquetReader, error) {
	ff := Fields()
	pr := &ParquetReader{
		fields: getFields(ff),
		r:      r,
	}

	for _, opt := range opts {
		opt(pr)
	}

	schema := make([]parquet.Field, len(ff))
	for i, f := range ff {
		schema[i] = f.Schema()
	}

	pr.meta = parquet.New(schema...)
	if err := pr.meta.ReadFooter(r); err != nil {
		return nil, err
	}
	pr.rows = pr.meta.Rows()
	var err error
	pr.offsets, err = pr.meta.Offsets()
	if err != nil {
		return nil, err
	}

	_, err = r.Seek(4, io.SeekStart)
	if err != nil {
		return nil, err
	}

	for i, rg := range pr.meta.RowGroups() {
		for _, col := range rg.Columns {
			name := col.MetaData.PathInSchema[len(col.MetaData.PathInSchema)-1]
			f, ok := pr.fields[name]
			if !ok {
				return nil, fmt.Errorf("unknown field: %s", name)
			}
			offsets := pr.offsets[f.Name()]
			if len(offsets) <= pr.index {
				break
			}

			o := offsets[i]
			if err := f.Read(r, pr.meta, o); err != nil {
				return nil, fmt.Errorf("unable to read field %s, err: %s", f.Name(), err)
			}
		}
	}
	return pr, nil
}

func readerIndex(i int) func(*ParquetReader) {
	return func(p *ParquetReader) {
		p.index = i
	}
}

// ParquetReader reads one page from a row group.
type ParquetReader struct {
	fields  map[string]Field
	index   int
	cur     int64
	rows    int64
	offsets map[string][]parquet.Position

	r    io.ReadSeeker
	meta *parquet.Metadata
}

func (p *ParquetReader) Rows() int64 {
	return p.meta.Rows()
}

func (p *ParquetReader) Next() bool {
	if p.cur >= p.rows {
		return false
	}
	p.cur++
	return true
}

func (p *ParquetReader) Scan(x *{{.Type}}) {
	for _, f := range p.fields {
		f.Scan(x)
	}
}`
