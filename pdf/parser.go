package pdf

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/KarmaPenny/pdfparser/logger"
	"io"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var start_xref_scan_buffer_size int64 = 256
var start_xref_regexp = regexp.MustCompile(`startxref\s*(\d+)\s*%%EOF`)
var start_obj_regexp = regexp.MustCompile(`\d+([\s\x00]|(%[^\n]*\n))+\d+([\s\x00]|(%[^\n]*\n))+obj`)
var xref_regexp = regexp.MustCompile(`xref`)
var whitespace = []byte("\x00\t\n\f\r ")
var delimiters = []byte("()<>[]/%")

func Parse(file_path string, password string, output_dir string) error {
	// create output dir
	os.RemoveAll(output_dir)
	os.MkdirAll(output_dir, 0755)

	// open the pdf
	file, err := os.Open(file_path)
	if err != nil {
		return err
	}
	defer file.Close()

	// create a new parser
	parser := NewParser(file)

	// load the pdf
	if err := parser.Load(password); err != nil {
		return err
	}

	// extract embedded files
	parser.ExtractFiles(output_dir)

	// extract text
	parser.ExtractText(output_dir)

	// TODO: extract javascript - might have to do this during dictionary parsing

	// TODO: extract urls - might have to do this during dictionary parsing

	// create raw.pdf file in output dir
	raw_pdf, err := os.Create(path.Join(output_dir, "raw.pdf"))
	defer raw_pdf.Close()

	// dump all objects to raw.pdf
	for object_number, xref_entry := range parser.Xref {
		if xref_entry.Type == XrefTypeIndirectObject {
			object := parser.GetObject(object_number)
			fmt.Fprintf(raw_pdf, "%d %d obj\n%s\n", object.Number, object.Generation, object.Value)
			if object.Stream != nil {
				fmt.Fprintf(raw_pdf, "stream\n%s\nendstream\n", string(object.Stream))
			}
			fmt.Fprintf(raw_pdf, "endobj\n\n")
		}
	}

	return nil
}

type Parser struct {
	*bufio.Reader
	seeker io.ReadSeeker
	Xref map[int]*XrefEntry
	trailer Dictionary
	security_handler *SecurityHandler
}

func NewParser(readSeeker io.ReadSeeker) *Parser {
	return &Parser{bufio.NewReader(readSeeker), readSeeker, map[int]*XrefEntry{}, Dictionary{}, defaultSecurityHandler}
}

func (parser *Parser) Load(password string) error {
	// find location of all xref tables
	logger.Debug("finding xref offsets")
	xref_offsets := parser.FindXrefOffsets()

	// add start xref offset to xref offsets
	logger.Debug("finding startxref offsets")
	if start_xref_offset, err := parser.GetStartXrefOffset(); err == nil {
		xref_offsets = append(xref_offsets, start_xref_offset)
	}

	// load all xrefs
	logger.Debug("loading xrefs")
	for i := range xref_offsets {
		parser.LoadXref(xref_offsets[i], map[int64]interface{}{})
	}

	// find location of all objects
	logger.Debug("finding objects")
	objects := parser.FindObjects()

	// repair broken and missing xref entries
	logger.Debug("repairing xrefs")
	for object_number, object := range objects {
		if xref_entry, ok := parser.Xref[object_number]; ok {
			// replace xref entry if it does not point to an object or points to the wrong object
			parser.Seek(xref_entry.Offset, io.SeekStart)
			if n, _, err := parser.ReadObjectHeader(); err != nil || n != object_number {
				xref_entry.Offset = object.Offset
			}
		} else {
			// add missing object to xref
			parser.Xref[object_number] = object
		}
	}

	// setup security handler if pdf is encrypted
	if encrypt, ok := parser.trailer["Encrypt"]; ok {
		logger.Debug("pdf is encrypted")
		// make sure we don't decrypt the encryption dictionary
		if ref, ok := encrypt.(*Reference); ok {
			if xref_entry, ok := parser.Xref[ref.Number]; ok {
				xref_entry.IsEncrypted = false
			}
		}

		// set the password
		if !parser.SetPassword(password) {
			logger.Debug("incorrect password")
			return ErrorPassword
		}
	}

	return nil
}

// FindXrefOffsets locates all xref tables
func (parser *Parser) FindXrefOffsets() []int64 {
	offsets := []int64{}

	// jump to start of file
	offset, _ := parser.Seek(0, io.SeekStart)

	for {
		// scan for xref table marker
		index := xref_regexp.FindReaderIndex(parser)
		if index == nil {
			break
		}

		// add location to offsets
		offsets = append(offsets, offset + int64(index[0]))

		// seek to end of xref marker
		offset, _ = parser.Seek(offset + int64(index[1]), io.SeekStart)
	}

	return offsets
}

// FindObjects locates all object markers
func (parser *Parser) FindObjects() map[int]*XrefEntry {
	// create xref map
	objects := map[int]*XrefEntry{}

	// jump to start of file
	offset, _ := parser.Seek(0, io.SeekStart)

	for {
		// scan for object start marker
		index := start_obj_regexp.FindReaderIndex(parser)
		if index == nil {
			break
		}

		// seek to start of object
		parser.Seek(offset + int64(index[0]), io.SeekStart)

		// get object number, generation
		n, g, _ := parser.ReadObjectHeader()

		// add xref entry
		objects[n] = NewXrefEntry(offset + int64(index[0]), g, XrefTypeIndirectObject)

		// seek to end of object start marker
		offset, _ = parser.Seek(offset + int64(index[1]), io.SeekStart)
	}

	return objects
}

func (parser *Parser) SetPassword(password string) bool {
	sh, err := NewSecurityHandler([]byte(password), parser.trailer)
	if err != nil {
		return false
	}
	parser.security_handler = sh
	return true
}

// GetStartXrefOffset returns the offset to the first xref table
func (parser *Parser) GetStartXrefOffset() (int64, error) {
	// start reading from the end of the file
	offset, _ := parser.Seek(0, io.SeekEnd)

	// dont start past begining of file
	offset -= start_xref_scan_buffer_size
	if offset < 0 {
		offset = 0
	}

	// read in buffer at offset
	buffer := make([]byte, start_xref_scan_buffer_size)
	parser.Seek(offset, io.SeekStart)
	parser.Read(buffer)

	// check for start xref
	matches := start_xref_regexp.FindAllSubmatch(buffer, -1)
	if matches != nil {
		// return the last most start xref offset
		start_xref_offset, err := strconv.ParseInt(string(matches[len(matches)-1][1]), 10, 64)
		if err != nil {
			return 0, WrapError(err, "Start xref offset is not int64: %s", string(matches[len(matches)-1][1]))
		}
		return start_xref_offset, nil
	}

	// start xref not found
	return 0, NewError("Start xref marker not found")
}

func (parser *Parser) LoadXref(offset int64, offsets map[int64]interface{}) error {
	// track loaded xref offsets to prevent infinite loop
	if _, ok := offsets[offset]; ok {
		// xref already loaded
		return nil
	}
	offsets[offset] = nil

	// if xref is a table
	parser.Seek(offset, io.SeekStart)
	if keyword := parser.ReadKeyword(); keyword == KEYWORD_XREF {
		return parser.LoadXrefTable(offsets)
	}

	// if xref is a stream
	parser.Seek(offset, io.SeekStart)
	if n, g, err := parser.ReadObjectHeader(); err == nil {
		// prevent decrypting xref streams
		parser.Xref[n] = NewXrefEntry(offset, g, XrefTypeIndirectObject)
		parser.Xref[n].IsEncrypted = false

		// read the xref object
		return parser.LoadXrefStream(n, offsets)
	}

	return NewError("Expected xref table or stream")
}

func (parser *Parser) LoadXrefTable(offsets map[int64]interface{}) error {
	// read all xref entries
	xrefs := map[int]*XrefEntry{}
	for {
		// get subsection start
		subsection_start, err := parser.ReadInt()
		if err != nil {
			// we are at the trailer
			if keyword := parser.ReadKeyword(); keyword == KEYWORD_TRAILER {
				break
			}
			return NewError("Expected int or trailer keyword")
		}

		// get subsection length
		subsection_length, err := parser.ReadInt()
		if err != nil {
			return err
		}

		// load each object in xref subsection
		for i := 0; i < subsection_length; i++ {
			// find xref entry offset
			offset, err := parser.ReadInt64()
			if err != nil {
				return err
			}

			// find xref entry generation
			generation, err := parser.ReadInt()
			if err != nil {
				return err
			}

			// find xref entry in use flag
			flag := parser.ReadKeyword()
			xref_type := XrefTypeFreeObject
			if flag == KEYWORD_N {
				xref_type = XrefTypeIndirectObject
			}

			// determine object number from subsection start
			object_number := subsection_start + i

			// add the object to xrefs
			xrefs[object_number] = NewXrefEntry(offset, generation, xref_type)
		}
	}

	// read in trailer dictionary
	trailer, err := parser.ReadDictionary(noDecryptor)
	if err != nil {
		return err
	}

	// load previous xref section if it exists
	if prev, err := trailer.GetInt64("Prev"); err == nil {
		parser.LoadXref(prev, offsets)
	}

	// merge trailer
	for key, value := range trailer {
		parser.trailer[key] = value
	}

	// merge xrefs
	for key, value := range xrefs {
		parser.Xref[key] = value
	}

	return nil
}

func (parser *Parser) LoadXrefStream(n int, offsets map[int64]interface{}) error {
	// Get the xref stream object
	object := parser.GetObject(n)

	// get the stream dictionary which is also the trailer dictionary
	trailer, ok := object.Value.(Dictionary)
	if !ok {
		return NewError("xref stream has no trailer dictionary")
	}

	// load previous xref section if it exists
	if prev, err := trailer.GetInt64("Prev"); err == nil {
		parser.LoadXref(prev, offsets)
	}

	// merge trailer
	for key, value := range trailer {
		parser.trailer[key] = value
	}

	// get the index and width arrays
	index, err := trailer.GetArray("Index")
	if err != nil {
		// if there is no Index field then use default of [0 Size]
		size, err := trailer.GetNumber("Size")
		if err != nil {
			return err
		}
		index = Array{Number(0), size}
	}
	width, err := trailer.GetArray("W")
	if err != nil {
		return err
	}

	// get widths of each field
	type_width, err := width.GetInt(0)
	if err != nil {
		return err
	}
	offset_width, err := width.GetInt(1)
	if err != nil {
		return err
	}
	generation_width, err := width.GetInt(2)
	if err != nil {
		return err
	}

	// parse xref subsections
	data_reader := bytes.NewReader(object.Stream)
	for i := 0; i < len(index) - 1; i += 2 {
		// get subsection start and length
		subsection_start, err := index.GetInt(i)
		if err != nil {
			return err
		}
		subsection_length, err := index.GetInt(i + 1)
		if err != nil {
			return err
		}

		// read in each entry in subsection
		for j := 0; j < subsection_length; j++ {
			xref_type, err := ReadInt(data_reader, type_width)
			if err != nil {
				return err
			}
			offset, err := ReadInt64(data_reader, offset_width)
			if err != nil {
				return err
			}
			generation, err := ReadInt(data_reader, generation_width)
			if err != nil {
				return err
			}

			// determine object number from subsection_start
			object_number := subsection_start + j

			// add the object to the xrefs
			parser.Xref[object_number] = NewXrefEntry(offset, generation, xref_type)
		}
	}

	return nil
}

func (parser *Parser) ExtractText(extract_dir string) error {
	// create a manifest file to store file name relationships
	text_file, err := os.Create(path.Join(extract_dir, "contents.txt"))
	if err != nil {
		return err
	}
	defer text_file.Close()

	root, _ := parser.trailer.GetDictionary("Root")
	pages, _ := root.GetDictionary("Pages")
	parser.extractText(pages, map[int]interface{}{}, text_file)
	return nil
}

func (parser *Parser) extractText(d Dictionary, resolved_kids map[int]interface{}, text_file *os.File) {
	kids, _ := d.GetArray("Kids")
	for i := range kids {
		// prevent infinite resolve reference loop
		if r, ok := kids[i].(*Reference); ok {
			if _, resolved := resolved_kids[r.Number]; resolved {
				continue
			}
			resolved_kids[r.Number] = nil
		}

		kid, _ := kids.GetDictionary(i)
		parser.extractText(kid, resolved_kids, text_file)
	}

	// load all fonts
	resources, _ := d.GetDictionary("Resources")
	fonts, _ := resources.GetDictionary("Font")
	font_map := map[string]*Font{}
	for font := range fonts {
		fontd, _ := fonts.GetDictionary(font)
		cmap, _ := fontd.GetStream("ToUnicode")
		font_map[font] = NewFont(cmap)
	}

	// get contents
	contents, _ := d.GetStream("Contents")

	// create parser for parsing contents
	contents_parser := NewParser(bytes.NewReader(contents))

	// parse text
	for {
		// read next command
		command, _, err := contents_parser.ReadCommand()
		if err == ErrorRead {
			break
		}

		// start of text block
		if command == KEYWORD_TEXT {
			// initial font is none
			current_font := FontDefault

			for {
				command, operands, err := contents_parser.ReadCommand()
				// stop if end of stream or end of text block
				if err == ErrorRead || command == KEYWORD_TEXT_END {
					break
				}

				// handle font changes
				if command == KEYWORD_TEXT_FONT {
					font_name, _ := operands.GetName(len(operands) - 2)
					if font, ok := font_map[font_name]; ok {
						current_font = font
					} else {
						current_font = FontDefault
					}
				} else if command == KEYWORD_TEXT_SHOW_1 || command == KEYWORD_TEXT_SHOW_2 || command == KEYWORD_TEXT_SHOW_3 {
					// decode text with current font font
					s, _ := operands.GetString(len(operands) - 1)
					text_file.WriteString(current_font.Decode([]byte(s)))
					text_file.WriteString("\n")
				} else if command == KEYWORD_TEXT_POSITION {
					// decode positioned text with current font
					var sb strings.Builder
					a, _ := operands.GetArray(len(operands) - 1)
					for i := 0; i < len(a); i += 2 {
						s, _ := a.GetString(i)
						sb.WriteString(string(s))
					}
					text_file.WriteString(current_font.Decode([]byte(sb.String())))
					text_file.WriteString("\n")
				}
			}
		}
	}
}

func (parser *Parser) ExtractFiles(extract_dir string) error {
	// create a manifest file to store file name relationships
	manifest, err := os.Create(path.Join(extract_dir, "embedded_files.txt"))
	if err != nil {
		return err
	}
	defer manifest.Close()

	// extract all embedded files
	root, _ := parser.trailer.GetDictionary("Root")
	names, _ := root.GetDictionary("Names")
	embedded_files, _ := names.GetDictionary("EmbeddedFiles")
	parser.extractFiles(embedded_files, extract_dir, map[int]interface{}{}, manifest)
	return nil
}

func (parser *Parser) extractFiles(d Dictionary, extract_dir string, resolved_kids map[int]interface{}, manifest *os.File) {
	kids, _ := d.GetArray("Kids")
	for i := range kids {
		// prevent infinite resolve reference loop
		if r, ok := kids[i].(*Reference); ok {
			if _, resolved := resolved_kids[r.Number]; resolved {
				continue
			}
			resolved_kids[r.Number] = nil
		}

		kid, _ := kids.GetDictionary(i)
		parser.extractFiles(kid, extract_dir, resolved_kids, manifest)
	}

	names, _ := d.GetArray("Names")
	for i := 1; i < len(names); i += 2 {
		file_specification, _ := names.GetDictionary(i)

		// read in file data
		ef, _ := file_specification.GetDictionary("EF")
		file_data := []byte{}
		if file_reference, err := ef.GetReference("F"); err == nil {
			// mark object as embedded file so alternate decryption algorithms are used
			if xref_entry, ok := parser.Xref[file_reference.Number]; ok {
				xref_entry.IsEmbeddedFile = true
			}
			file_data = file_reference.ResolveStream()
		}

		// get md5 hash of the file
		hash := md5.New()
		hash.Write(file_data)
		md5sum := hex.EncodeToString(hash.Sum(nil))

		// add file name relationship to manifest
		file_name, _ := file_specification.GetString("F")
		fmt.Fprintf(manifest, "%s %s", md5sum, file_name)

		// write file data to file in extract dir
		ioutil.WriteFile(path.Join(extract_dir, md5sum), file_data, 0644)
	}
}

func (parser *Parser) GetObject(number int) *IndirectObject {
	logger.Debug("Reading object %d", number)

	object := NewIndirectObject(number)

	if xref_entry, ok := parser.Xref[number]; ok {
		if xref_entry.Type == XrefTypeIndirectObject {
			// set generation number
			object.Generation = xref_entry.Generation

			// seek to start of object
			parser.Seek(xref_entry.Offset, io.SeekStart)

			// skip object header
			parser.ReadObjectHeader()

			// initialize string decryption filter
			var string_filter CryptFilter = noFilter
			if parser.security_handler != nil {
				string_filter = parser.security_handler.string_filter
			}
			string_decryptor := string_filter.NewDecryptor(number, object.Generation)

			// get the value of the object
			logger.Debug("Reading object value")
			object.Value, _ = parser.ReadObject(string_decryptor)

			// get next keyword
			if keyword := parser.ReadKeyword(); keyword == KEYWORD_STREAM {
				logger.Debug("Reading object stream")
				// get stream dictionary
				d, ok := object.Value.(Dictionary)
				if !ok {
					d = Dictionary{}
				}

				// create list of decode filters
				filter_list, err := d.GetArray("Filter")
				if err != nil {
					if filter, err := d.GetName("Filter"); err == nil {
						filter_list = Array{Name(filter)}
					} else {
						filter_list = Array{}
					}
				}

				// create list of decode parms
				decode_parms_list, err := d.GetArray("DecodeParms")
				if err != nil {
					if decode_parms, err := d.GetDictionary("DecodeParms"); err == nil {
						decode_parms_list = Array{decode_parms}
					} else {
						decode_parms_list = Array{}
					}
				}

				// create a stream decryptor
				var crypt_filter CryptFilter = noFilter
				if parser.security_handler != nil && xref_entry.IsEncrypted {
					// use stream filter by default
					crypt_filter = parser.security_handler.stream_filter

					// use embedded file filter if object is an embedded file
					if xref_entry.IsEmbeddedFile {
						crypt_filter = parser.security_handler.file_filter
					}

					// handle crypt filter override
					if len(filter_list) > 0 {
						if filter, _ := filter_list.GetName(0); filter == "Crypt" {
							decode_parms, _ := decode_parms_list.GetDictionary(0)
							filter_name, err := decode_parms.GetName("Name")
							if err != nil {
								filter_name = "Identity"
							}
							if cf, exists := parser.security_handler.crypt_filters[filter_name]; exists {
								crypt_filter = cf
							}
							filter_list = filter_list[1:]
							if len(decode_parms_list) > 0 {
								decode_parms_list = decode_parms_list[1:]
							}
						}
					}
				}
				stream_decryptor := crypt_filter.NewDecryptor(number, xref_entry.Generation)

				// read the stream
				object.Stream = parser.ReadStream(stream_decryptor, filter_list, decode_parms_list)
			}
		}
	}

	return object
}

func (parser *Parser) Seek(offset int64, whence int) (int64, error) {
	parser.Reset(parser.seeker)
	return parser.seeker.Seek(offset, whence)
}

func (parser *Parser) CurrentOffset() int64 {
	offset, err := parser.seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}
	return offset - int64(parser.Buffered())
}

// ReadObjectHeader reads an object header (10 0 obj) from the current position and returns the object number and generation
func (parser *Parser) ReadObjectHeader() (int, int, error) {
	// read object number
	number, err := parser.ReadInt()
	if err != nil {
		return number, 0, err
	}

	// read object generation
	generation, err := parser.ReadInt()
	if err != nil {
		return number, generation, err
	}

	// read object start marker
	if keyword := parser.ReadKeyword(); keyword != KEYWORD_OBJ {
		return number, generation, NewError("Expected obj keyword")
	}
	return number, generation, nil
}

func (parser *Parser) ReadObject(decryptor Decryptor) (Object, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// peek at next 2 bytes to determine object type
	b, _ := parser.Peek(2)
	if len(b) == 0 {
		return KEYWORD_NULL, ErrorRead
	}

	// handle names
	if b[0] == '/' {
		return parser.ReadName()
	}

	// handle arrays
	if b[0] == '[' {
		return parser.ReadArray(decryptor)
	}
	if b[0] == ']' {
		parser.Discard(1)
		return KEYWORD_NULL, EndOfArray
	}

	// handle strings
	if b[0] == '(' {
		return parser.ReadString(decryptor)
	}

	// handle dictionaries
	if string(b) == "<<" {
		return parser.ReadDictionary(decryptor)
	}
	if string(b) == ">>" {
		parser.Discard(2)
		return KEYWORD_NULL, EndOfDictionary
	}

	// handle hex strings
	if b[0] == '<' {
		return parser.ReadHexString(decryptor)
	}

	// handle numbers and references
	if (b[0] >= '0' && b[0] <= '9') || b[0] == '+' || b[0] == '-' || b[0] == '.' {
		number, err := parser.ReadNumber()
		if err != nil {
			return number, err
		}

		// save offset so we can revert if this is not a reference
		offset := parser.CurrentOffset()

		// if generation number does not follow then revert to saved offset and return number
		generation, err := parser.ReadInt()
		if err != nil {
			parser.Seek(offset, io.SeekStart)
			return number, nil
		}

		// if not a reference then revert to saved offset and return the number
		if keyword := parser.ReadKeyword(); keyword != KEYWORD_R {
			parser.Seek(offset, io.SeekStart)
			return number, nil
		}

		// return the reference
		return NewReference(parser, int(number), generation), nil
	}

	// handle keywords
	return parser.ReadKeyword(), nil
}

func (parser *Parser) ReadArray(decryptor Decryptor) (Array, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create a new array
	array := Array{}

	// read start of array marker
	b, err := parser.ReadByte()
	if err != nil {
		return array, ErrorRead
	}
	if b != '[' {
		return array, NewError("Expected [")
	}

	// read in elements and append to array
	for {
		element, err := parser.ReadObject(decryptor)
		if err == ErrorRead || err == EndOfArray {
			// stop if at eof or end of array
			break
		}
		array = append(array, element)
	}

	// return array
	return array, nil
}

func (parser *Parser) ReadCommand() (Keyword, Array, error) {
	// read in operands until command keyword
	operands := Array{}
	for {
		operand, err := parser.ReadObject(noDecryptor)
		if err == ErrorRead {
			return KEYWORD_NULL, operands, err
		}
		if keyword, ok := operand.(Keyword); ok {
			return keyword, operands, nil
		}
		operands = append(operands, operand)
	}
}

func (parser *Parser) ReadDictionary(decryptor Decryptor) (Dictionary, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create new dictionary
	dictionary := Dictionary{}

	// read start of dictionary markers
	b := make([]byte, 2)
	_, err := parser.Read(b)
	if err != nil {
		return dictionary, ErrorRead
	}
	if string(b) != "<<" {
		return dictionary, NewError("Expected <<")
	}

	// parse all key value pairs
	for {
		// read next object
		name, err := parser.ReadObject(decryptor)
		if err == ErrorRead || err == EndOfDictionary {
			break
		}
		key, isName := name.(Name)
		if !isName || err != nil {
			continue
		}

		// get value
		value, err := parser.ReadObject(decryptor)

		// add key value pair to dictionary
		dictionary[string(key)] = value

		// stop if at eof or end of dictionary
		if err == ErrorRead || err == EndOfDictionary {
			break
		}
	}
	return dictionary, nil
}

func (parser *Parser) ReadHexString(decryptor Decryptor) (String, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create new string builder
	var s strings.Builder

	// read start of hex string marker
	b, err := parser.ReadByte()
	if err != nil {
		return String(s.String()), ErrorRead
	}
	if b != '<' {
		return String(s.String()), NewError("Expected <")
	}

	// read hex code pairs until end of hex string or file
	for {
		code := []byte{'0', '0'}
		for i := 0; i < 2; {
			parser.consumeWhitespace()
			b, err := parser.ReadByte()
			if err != nil || b == '>' {
				if i > 0 {
					val, _ := strconv.ParseUint(string(code), 16, 8)
					s.WriteByte(byte(val))
				}
				return String(decryptor.Decrypt([]byte(s.String()))), nil
			}
			if !IsHex(b) {
				continue
			}
			code[i] = b
			i++
		}
		val, _ := strconv.ParseUint(string(code), 16, 8)
		s.WriteByte(byte(val))
	}
}

func (parser *Parser) ReadInt() (int, error) {
	value, err := parser.ReadInt64()
	return int(value), err
}

func (parser *Parser) ReadInt64() (int64, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create a new number object
	value := int64(0)

	// ensure first byte is a digit
	b, err := parser.ReadByte()
	if err != nil || b < '0' || b > '9' {
		parser.UnreadByte()
		return value, NewError("Expected int")
	}

	// add digit to value
	value = value * 10 + int64(b - '0')

	// parse int part
	for {
		b, err = parser.ReadByte()
		if err != nil {
			break
		}

		// stop if no numeric char
		if b < '0' || b > '9' {
			parser.UnreadByte()
			break
		}

		// add digit to value
		value = value * 10 + int64(b - '0')
	}

	return value, nil
}

func (parser *Parser) ReadKeyword() Keyword {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// build keyword
	var keyword strings.Builder

	for {
		// read in the next byte
		b, err := parser.ReadByte()
		if err != nil {
			break
		}

		// stop if not keyword character
		if bytes.IndexByte(whitespace, b) >= 0 || bytes.IndexByte(delimiters, b) >= 0 {
			parser.UnreadByte()
			break
		}

		// add character to keyword
		keyword.WriteByte(b)
	}

	// interpret keyword value
	return NewKeyword(keyword.String())
}

func (parser *Parser) ReadName() (Name, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// build name
	var name strings.Builder

	// read start of name marker
	b, err := parser.ReadByte()
	if err != nil {
		return Name(name.String()), ErrorRead
	}
	if b != '/' {
		return Name(name.String()), NewError("Expected /")
	}

	for {
		// read in the next byte
		b, err = parser.ReadByte()
		if err != nil {
			return Name(name.String()), nil
		}

		// if the next byte is whitespace or delimiter then unread it and return the name
		if bytes.IndexByte(delimiters, b) >= 0 || bytes.IndexByte(whitespace, b) >= 0 {
			parser.UnreadByte()
			break
		}

		// if next byte is the start of a hex character code
		if b == '#' {
			// read in the hex code
			code := []byte{'0', '0'}
			for i := 0; i < 2; i++ {
				b, err = parser.ReadByte()
				if err != nil {
					break
				}
				if !IsHex(b) {
					parser.UnreadByte()
					break
				}
				code[i] = b
			}

			// convert the hex code to a byte
			val, _ := strconv.ParseUint(string(code), 16, 8)
			b = byte(val)
		}

		// add byte to name
		name.WriteByte(b)
	}

	return Name(name.String()), nil
}

func (parser *Parser) ReadNumber() (Number, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create a new number object
	var number Number
	isReal := false
	isNegative := false

	// process first byte
	b, err := parser.ReadByte()
	if err != nil {
		return number, ErrorRead
	}
	if b == '-' {
		isNegative = true
	} else if b >= '0' && b <= '9' {
		number = Number(float64(number) * 10 + float64(b - '0'))
	} else if b == '.' {
		isReal = true
	} else if b != '+' {
		parser.UnreadByte()
		return number, NewError("Expected number")
	}

	// parse int part
	for !isReal {
		b, err = parser.ReadByte()
		if err != nil {
			break
		}

		if b >= '0' && b <= '9' {
			number = Number(float64(number) * 10 + float64(b - '0'))
		} else if b == '.' {
			isReal = true
		} else {
			parser.UnreadByte()
			break
		}
	}

	// parse real part
	if isReal {
		for i := 1; true; i++ {
			b, err = parser.ReadByte()
			if err != nil {
				break
			}

			if b >= '0' && b <= '9' {
				number = Number(float64(number) + float64(b - '0') / (10 * float64(i)))
			} else {
				parser.UnreadByte()
				break
			}
		}
	}

	// make negative if first byte was a minus sign
	if isNegative {
		number = -number
	}

	// return the number
	return number, nil
}

func (parser *Parser) ReadStream(decryptor Decryptor, filter_list Array, decode_parms_list Array) []byte {
	// create buffers for stream data
	stream_data := bytes.NewBuffer([]byte{})

	// read until new line
	for {
		b, err := parser.ReadByte()
		if err != nil {
			return stream_data.Bytes()
		}

		// if new line then we are at the start of the stream data
		if b == '\n' {
			break
		}

		// if carriage return check if next byte is line feed
		if b == '\r' {
			b, err := parser.ReadByte()
			if err != nil {
				return stream_data.Bytes()
			}
			// if not new line then put it back cause it is part of the stream data
			if b != '\n' {
				parser.UnreadByte()
			}
			break
		}
	}

	// read first 9 bytes to get started
	end_buff := bytes.NewBuffer([]byte{})
	buff := make([]byte, 9)
	bytes_read, _ := parser.Read(buff)
	if bytes_read > 0 {
		end_buff.Write(buff[:bytes_read])
	}

	// read in stream data until endstream marker
	for {
		if end_buff.String() == "endstream" {
			// truncate last new line from stream_data and stop reading stream data
			l := stream_data.Len()
			if l-1 >= 0 && stream_data.Bytes()[l-1] == '\n' {
				if l-2 >= 0 && stream_data.Bytes()[l-2] == '\r' {
					stream_data.Truncate(l-2)
				} else {
					stream_data.Truncate(l-1)
				}
			} else if l-1 >= 0 && stream_data.Bytes()[l-1] == '\r' {
				stream_data.Truncate(l-1)
			}
			break
		}

		// add first byte of end_buff to stream_data
		b, err := end_buff.ReadByte()
		if err != nil {
			break
		}
		stream_data.WriteByte(b)

		// add next byte of stream to end_buff
		b, err = parser.ReadByte()
		if err != nil {
			stream_data.Write(end_buff.Bytes())
			break
		}
		end_buff.WriteByte(b)
	}

	// decrypt stream
	stream_data_bytes := decryptor.Decrypt(stream_data.Bytes())

	// decode stream
	for i := 0; i < len(filter_list); i++ {
		filter, _ := filter_list.GetName(i)
		decode_parms, _ := decode_parms_list.GetDictionary(i)
		decoded_bytes, err := DecodeStream(filter, stream_data_bytes, decode_parms)
		if err != nil {
			// stop when decode error enountered
			return stream_data_bytes
		}
		stream_data_bytes = decoded_bytes
	}

	// return the decrypted and decoded stream
	return stream_data_bytes
}

func (parser *Parser) ReadString(decryptor Decryptor) (String, error) {
	// consume any leading whitespace/comments
	parser.consumeWhitespace()

	// create new string builder
	var s strings.Builder

	// read start of string marker
	b, err := parser.ReadByte()
	if err != nil {
		return String(s.String()), ErrorRead
	}
	if b != '(' {
		return String(s.String()), NewError("Expected (")
	}

	// find balanced closing bracket
	for open_parens := 1; true; {
		// read next byte
		b, err = parser.ReadByte()
		if err != nil {
			return String(decryptor.Decrypt([]byte(s.String()))), nil
		}

		// if this is the start of an escape sequence
		if b == '\\' {
			// read next byte
			b, err = parser.ReadByte()
			if err != nil {
				s.WriteByte('\\')
				return String(decryptor.Decrypt([]byte(s.String()))), nil
			}

			// ignore escaped line breaks \n or \r or \r\n
			if b == '\n' {
				continue
			}
			if b == '\r' {
				// read next byte
				b, err = parser.ReadByte()
				if err != nil {
					return String(decryptor.Decrypt([]byte(s.String()))), nil
				}
				// if byte is not a new line then unread it
				if b != '\n' {
					parser.UnreadByte()
				}
				continue
			}

			// special escape values
			if b == 'n' {
				b = '\n'
			} else if b == 'r' {
				b = '\r'
			} else if b == 't' {
				b = '\t'
			} else if b == 'b' {
				b = '\b'
			} else if b == 'f' {
				b = '\f'
			}

			// if this is the start of an octal character code
			if b >= '0' && b <= '7' {
				// add byte to character code
				code := bytes.NewBuffer([]byte{b})

				// add at most 2 more bytes to code
				for i := 0; i < 2; i++ {
					// read next byte
					b, err = parser.ReadByte()
					if err != nil {
						break
					}

					// if next byte is not part of the octal code
					if b < '0' || b > '7' {
						// unread the byte and stop collecting code
						parser.UnreadByte()
						break
					}

					// add byte to code
					code.WriteByte(b)
				}

				// convert code into byte
				val, err := strconv.ParseUint(string(code.Bytes()), 8, 8)
				if err != nil {
					// octal code is too large so ignore last byte
					parser.UnreadByte()
					val, _ = strconv.ParseUint(string(code.Bytes()[:code.Len()-1]), 8, 8)
				}
				b = byte(val)
			}

			// add byte to string and continue
			s.WriteByte(b)
			continue
		}

		// keep track of number of open parens
		if b == '(' {
			open_parens++
		} else if b == ')' {
			open_parens--
		}

		// stop if last paren was read
		if open_parens == 0 {
			break
		}

		// add byte to string
		s.WriteByte(b)
	}

	// return string
	return String(decryptor.Decrypt([]byte(s.String()))), nil
}

// consumeWhitespace reads until end of whitespace/comments
func (parser *Parser) consumeWhitespace() {
	for {
		// get next byte
		b, err := parser.ReadByte()
		if err != nil {
			return
		}

		// consume comments and whitespace
		if b == '%' {
			parser.consumeComment()
		} else if bytes.IndexByte(whitespace, b) < 0 {
			parser.UnreadByte()
			return
		}
	}
}

func (parser *Parser) consumeComment() {
	for {
		// get next byte
		b, err := parser.ReadByte()
		if err != nil {
			return
		}

		// stop on line feed
		if b == '\n' {
			return
		}

		// stop on carriage return
		if b == '\r' {
			// consume optional line feed
			b, err := parser.ReadByte()
			if err != nil {
				return
			}
			if b != '\n' {
				parser.UnreadByte()
			}
			return
		}
	}
}
