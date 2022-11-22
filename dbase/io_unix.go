//go:build !windows
// +build !windows

package dbase

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var defaultIO UnixIO

type UnixIO struct{}

/**
 *	################################################################
 *	#					IO Functions
 *	################################################################
 */

// Opens a dBase database file (and the memo file if needed) from disk.
// To close the embedded file handle(s) call DBF.Close().
func (u UnixIO) OpenTable(config *Config) (*File, error) {
	if config == nil {
		return nil, newError("dbase-io-opentable-1", fmt.Errorf("missing configuration"))
	}
	debugf("Opening table: %s - Exclusive: %v - Untested: %v - Trim spaces: %v - Write lock: %v - ValidateCodepage: %v - InterpretCodepage: %v", config.Filename, config.Exclusive, config.Untested, config.TrimSpaces, config.WriteLock, config.ValidateCodePage, config.InterpretCodePage)
	if len(strings.TrimSpace(config.Filename)) == 0 {
		return nil, newError("dbase-io-opentable-2", fmt.Errorf("missing filename"))
	}
	fileExtension := FileExtension(strings.ToUpper(filepath.Ext(config.Filename)))
	fileName := filepath.Clean(config.Filename)
	fileName, err := _findFile(fileName)
	if err != nil {
		return nil, newError("dbase-io-opentable-3", err)
	}
	mode := os.O_RDWR
	if config.Exclusive {
		mode |= os.O_EXCL
	}
	handle, err := os.OpenFile(fileName, mode, 0600)
	if err != nil {
		return nil, newError("dbase-io-opentable-4", fmt.Errorf("opening file failed with error: %w", err))
	}
	file := &File{
		config:     config,
		io:         u,
		handle:     handle,
		dbaseMutex: &sync.Mutex{},
		memoMutex:  &sync.Mutex{},
	}
	err = u.ReadHeader(file)
	if err != nil {
		return nil, newError("dbase-io-preparedbf-1", err)
	}
	// Check if the fileversion flag is expected, expand validFileVersion if needed
	if err := validateFileVersion(file.header.FileType, config.Untested); err != nil {
		return nil, newError("dbase-io-preparedbf-2", err)
	}
	columns, nullFlag, err := u.ReadColumns(file)
	if err != nil {
		return nil, newError("dbase-io-preparedbf-3", err)
	}
	file.nullFlagColumn = nullFlag
	file.table = &Table{
		columns: columns,
		mods:    make([]*Modification, len(columns)),
	}
	// Interpret the code page mark if needed
	if config.InterpretCodePage || config.Converter == nil {
		if config.Converter == nil {
			debugf("No encoding converter defined, falling back to default (interpreting)")
		}
		debugf("Interpreting code page mark...")
		file.config.Converter = ConverterFromCodePage(file.header.CodePage)
		debugf("Code page: 0x%02x => interpreted: 0x%02x", file.header.CodePage, file.config.Converter.CodePage())
	}
	// Check if the code page mark is matchin the converter
	if config.ValidateCodePage && file.header.CodePage != file.config.Converter.CodePage() {
		return nil, newError("dbase-io-opentable-6", fmt.Errorf("code page mark mismatch: %d != %d", file.header.CodePage, file.config.Converter.CodePage()))
	}
	// Check if there is an FPT according to the header.
	// If there is we will try to open it in the same dir (using the same filename and case).
	// If the FPT file does not exist an error is returned.
	if MemoFlag.Defined(file.header.TableFlags) {
		ext := FPT
		if fileExtension == DBC {
			ext = DCT
		}
		relatedFile := strings.TrimSuffix(fileName, path.Ext(fileName)) + string(ext)
		debugf("Opening related file: %s\n", relatedFile)
		relatedHandle, err := os.OpenFile(relatedFile, mode, 0600)
		if err != nil {
			return nil, newError("dbase-io-opentable-7", fmt.Errorf("opening FPT file failed with error: %w", err))
		}
		file.relatedHandle = relatedHandle
		err = file.ReadMemoHeader()
		if err != nil {
			return nil, newError("dbase-io-opentable-8", err)
		}
	}
	return file, nil
}

// Closes the file handlers.
func (u UnixIO) Close(file *File) error {
	if file.handle != nil {
		handle, ok := file.handle.(*os.File)
		if !ok {
			return newError("dbase-io-close-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
		}

		debugf("Closing file: %s", file.config.Filename)
		err := handle.Close()
		if err != nil {
			return newError("dbase-io-close-2", fmt.Errorf("closing DBF failed with error: %w", err))
		}
	}
	if file.relatedHandle != nil {
		relatedHandle, ok := file.relatedHandle.(*os.File)
		if !ok {
			return newError("dbase-io-close-3", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
		}

		debugf("Closing related file: %s", file.config.Filename)
		err := relatedHandle.Close()
		if err != nil {
			return newError("dbase-io-close-4", fmt.Errorf("closing FPT failed with error: %w", err))
		}
	}
	return nil
}

/**
 *	################################################################
 *	#				dBase database file IO handler
 *	################################################################
 */

func (u UnixIO) Create(file *File) error {
	file.config.Filename = strings.ToUpper(strings.TrimSpace(file.config.Filename))
	// Check for valid file name
	if len(file.config.Filename) == 0 {
		return newError("dbase-io-create-1", fmt.Errorf("missing filename"))
	}
	// Check for valid file extension
	if filepath.Ext(strings.ToUpper(file.config.Filename)) != ".DBF" {
		return newError("dbase-io-create-2", fmt.Errorf("invalid file extension"))
	}
	// Check if file exists already
	if _, err := os.Stat(file.config.Filename); err == nil {
		return newError("dbase-io-create-3", fmt.Errorf("file already exists"))
	}
	// Create the file
	debugf("Creating file: %s", file.config.Filename)
	handle, err := os.Create(strings.ToUpper(file.config.Filename))
	if err != nil {
		return newError("dbase-io-create-4", fmt.Errorf("creating DBF file failed with error: %w", err))
	}
	file.handle = handle
	if file.memoHeader != nil {
		debugf("Creating related file: %s", file.config.Filename)
		// Create the memo file
		relatedHandle, err := os.Create(strings.TrimSuffix(file.config.Filename, filepath.Ext(file.config.Filename)) + ".FPT")
		if err != nil {
			return newError("dbase-io-create-5", fmt.Errorf("creating FPT file failed with error: %w", err))
		}
		file.relatedHandle = relatedHandle
	}
	return nil
}

// Reads the DBF header from the file handle.
func (u UnixIO) ReadHeader(file *File) error {
	debugf("Reading header...")
	handle, ok := file.handle.(*os.File)
	if !ok {
		return newError("dbase-io-readheader-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	h := &Header{}
	if _, err := handle.Seek(0, 0); err != nil {
		return newError("dbase-io-readheader-2", err)
	}
	b := make([]byte, 30)
	n, err := handle.Read(b)
	if err != nil {
		return newError("dbase-io-readheader-3", err)
	}
	// LittleEndian - Integers in table files are stored with the least significant byte first.
	err = binary.Read(bytes.NewReader(b[:n]), binary.LittleEndian, h)
	if err != nil {
		return newError("dbase-io-readheader-4", err)
	}
	file.header = h
	return nil
}

// writeHeader writes the header to the dbase file
func (u UnixIO) WriteHeader(file *File) (err error) {
	debugf("Writing header - exclusive writing: %v", file.config.WriteLock)
	handle, ok := file.handle.(*os.File)
	if !ok {
		return newError("dbase-io-writeheader-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	// Lock the block we are writing to
	if file.config.WriteLock {
		flock := &unix.Flock_t{
			Type:   unix.F_WRLCK,
			Start:  0,
			Len:    int64(file.header.FirstRow),
			Whence: 0,
		}
		for {
			err = unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EAGAIN) {
				return newError("dbase-io-writeheader-1", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			flock.Type = unix.F_ULOCK
			ulockErr := unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if ulockErr != nil {
				err = newError("dbase-io-writeheader-2", ulockErr)
			}
		}()
	}
	// Seek to the beginning of the file
	_, err = handle.Seek(0, 0)
	if err != nil {
		return newError("dbase-io-writeheader-3", err)
	}
	// Change the last modification date to the current date
	file.header.Year = uint8(time.Now().Year() - 2000)
	file.header.Month = uint8(time.Now().Month())
	file.header.Day = uint8(time.Now().Day())
	debugf("Writing header: %+v", file.header)
	// Write the header
	buf := new(bytes.Buffer)
	err = binary.Write(buf, binary.LittleEndian, file.header)
	if err != nil {
		return newError("dbase-io-writeheader-4", err)
	}
	_, err = handle.Write(buf.Bytes())
	if err != nil {
		return newError("dbase-io-writeheader-5", err)
	}
	return nil
}

// Reads column infos from DBF header, starting at pos 32, until it finds the Header row terminator END_OF_COLUMN(0x0D).
func (u UnixIO) ReadColumns(file *File) ([]*Column, *Column, error) {
	debugf("Reading columns...")
	handle, ok := file.handle.(*os.File)
	if !ok {
		return nil, nil, newError("dbase-io-readcolumns-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	var nullFlag *Column
	columns := make([]*Column, 0)
	offset := int64(32)
	b := make([]byte, 1)
	for {
		// Check if we are at 0x0D by reading one byte ahead
		if _, err := handle.Seek(offset, 0); err != nil {
			return nil, nil, newError("dbase-io-readcolumninfos-1", err)
		}
		if _, err := handle.Read(b); err != nil {
			return nil, nil, newError("dbase-io-readcolumninfos-2", err)
		}
		if b[0] == byte(ColumnEnd) {
			break
		}
		// Position back one byte and read the column
		if _, err := handle.Seek(-1, 1); err != nil {
			return nil, nil, newError("dbase-io-readcolumninfos-3", err)
		}
		buf := make([]byte, 33)
		n, err := handle.Read(buf)
		if err != nil {
			return nil, nil, newError("dbase-io-readcolumninfos-4", err)
		}
		column := &Column{}
		err = binary.Read(bytes.NewReader(buf[:n]), binary.LittleEndian, column)
		if err != nil {
			return nil, nil, newError("dbase-io-readcolumninfos-5", err)
		}
		if column.Name() == "_NullFlags" {
			debugf("Found null flag column: %s", column.Name())
			nullFlag = column
			offset += 32
			continue
		}
		debugf("Found column %v of type %v at offset: %d", column.Name(), column.Type(), offset)
		columns = append(columns, column)
		offset += 32
	}
	return columns, nullFlag, nil
}

func (u UnixIO) WriteColumns(file *File) (err error) {
	debugf("Writing columns - exclusive writing: %v", file.config.WriteLock)
	handle, ok := file.handle.(*os.File)
	if !ok {
		return newError("dbase-io-writecolumns-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	// Lock the block we are writing to
	position := uint32(32)
	// Lock the block we are writing to
	if file.config.WriteLock {
		flock := &unix.Flock_t{
			Type:   unix.F_WRLCK,
			Start:  int64(position),
			Len:    int64(file.header.FirstRow),
			Whence: 0,
		}
		for {
			err = unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EAGAIN) {
				return newError("dbase-io-writecolumns-1", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			flock.Type = unix.F_ULOCK
			ulockErr := unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if ulockErr != nil {
				err = newError("dbase-io-writecolumns-2", ulockErr)
			}
		}()
	}
	// Seek to the beginning of the file
	_, err = handle.Seek(32, 0)
	if err != nil {
		return newError("dbase-io-writecolumns-3", err)
	}
	// Write the columns
	buf := new(bytes.Buffer)
	for _, column := range file.table.columns {
		debugf("Writing column: %+v", column)
		err = binary.Write(buf, binary.LittleEndian, column)
		if err != nil {
			return newError("dbase-io-writecolumns-4", err)
		}
	}
	if file.nullFlagColumn != nil {
		debugf("Writing null flag column: %s", file.nullFlagColumn.Name())
		err = binary.Write(buf, binary.LittleEndian, file.nullFlagColumn)
		if err != nil {
			return newError("dbase-io-writecolumns-5", err)
		}
	}
	_, err = handle.Write(buf.Bytes())
	if err != nil {
		return newError("dbase-io-writecolumns-6", err)
	}
	// Write the column terminator
	_, err = handle.Write([]byte{byte(ColumnEnd)})
	if err != nil {
		return newError("dbase-io-writecolumns-7", err)
	}
	// Write null till the end of the header
	pos := file.header.FirstRow - uint16(len(file.table.columns)*32) - 33
	if file.nullFlagColumn != nil {
		pos -= 32
	}
	_, err = handle.Write(make([]byte, pos))
	if err != nil {
		return newError("dbase-io-writecolumns-8", err)
	}
	return nil
}

// Read the nullFlag field at the end of the row
// The nullFlag field indicates if the field has a variable length
// If varlength is true, the field is variable length and the length is stored in the last byte
// If varlength is false, we read the complete field
// If the field is null, we return true as second return value
func (u UnixIO) ReadNullFlag(file *File, rowPosition uint64, column *Column) (bool, bool, error) {
	handle, ok := file.handle.(*os.File)
	if !ok {
		return false, false, newError("dbase-io-readnullflag-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	if file.nullFlagColumn == nil {
		return false, false, newError("dbase-io-readnullflag-2", fmt.Errorf("null flag column not found"))
	}
	if column.DataType != byte(Varchar) && column.DataType != byte(Varbinary) {
		return false, false, newError("dbase-io-readnullflag-3", fmt.Errorf("column is not a varchar or varbinary column"))
	}
	// count what number of varchar field this field is
	bitCount := 0
	for _, c := range file.table.columns {
		if c.DataType == byte(Varchar) || c.DataType == byte(Varbinary) {
			if c == column {
				break
			}
			if c.Flag == byte(NullableFlag) || c.Flag == byte(NullableFlag|BinaryFlag) {
				bitCount += 2
			} else {
				bitCount++
			}
		}
	}
	// Read the null flag field
	position := uint64(file.header.FirstRow) + rowPosition*uint64(file.header.RowLength) + uint64(file.nullFlagColumn.Position)
	_, err := handle.Seek(int64(position), 0)
	if err != nil {
		return false, false, newError("dbase-io-readnullflag-1", err)
	}
	buf := make([]byte, file.nullFlagColumn.Length)
	n, err := handle.Read(buf)
	if err != nil {
		return false, false, newError("dbase-io-readnullflag-2", err)
	}
	if n != int(file.nullFlagColumn.Length) {
		return false, false, newError("dbase-io-readnullflag-3", fmt.Errorf("read %d bytes, expected %d", n, file.nullFlagColumn.Length))
	}

	if column.Flag == byte(NullableFlag) || column.Flag == byte(NullableFlag|BinaryFlag) {
		debugf("Read _NullFlag for column %s => varlength: %v - null: %v", column.Name(), nthBit(buf, bitCount), nthBit(buf, bitCount+1))
		return nthBit(buf, bitCount), nthBit(buf, bitCount+1), nil
	}

	debugf("Read _NullFlag for column %s => varlength: %v", column.Name(), nthBit(buf, bitCount))
	return nthBit(buf, bitCount), false, nil
}

/**
 *	################################################################
 *	#				Memo file IO handler
 *	################################################################
 */

// readMemoHeader reads the memo header from the given file handle.
func (u UnixIO) ReadMemoHeader(file *File) error {
	debugf("Reading memo header...")
	handle, ok := file.relatedHandle.(*os.File)
	if !ok {
		return newError("dbase-io-readmemoheader-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.relatedHandle))
	}
	h := &MemoHeader{}
	if _, err := handle.Seek(0, 0); err != nil {
		return newError("dbase-io-readmemoheader-2", err)
	}
	b := make([]byte, 1024)
	n, err := handle.Read(b)
	if err != nil {
		return newError("dbase-io-readmemoheader-3", err)
	}
	err = binary.Read(bytes.NewReader(b[:n]), binary.BigEndian, h)
	if err != nil {
		return newError("dbase-io-readmemoheader-4", err)
	}
	debugf("Memo header: %+v", h)
	file.relatedHandle = handle
	file.memoHeader = h
	return nil
}

// Reads one or more blocks from the FPT file, called for each memo column.
// the return value is the raw data and true if the data read is text (false is RAW binary data).
func (u UnixIO) ReadMemo(file *File, blockdata []byte) ([]byte, bool, error) {
	if file.relatedHandle == nil {
		return nil, false, newError("dbase-io-readmemo-1", ErrNoFPT)
	}
	handle, ok := file.relatedHandle.(*os.File)
	if !ok {
		return nil, false, newError("dbase-io-readmemo-2", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	// Determine the block number
	block := binary.LittleEndian.Uint32(blockdata)
	// The position in the file is blocknumber*blocksize
	position := int64(file.memoHeader.BlockSize) * int64(block)
	debugf("Reading memo block %d at position %d", block, position)
	_, err := handle.Seek(position, 0)
	if err != nil {
		return nil, false, newError("dbase-io-readmemo-2", err)
	}
	// Read the memo block header, instead of reading into a struct using binary.Read we just read the two
	// uints in one buffer and then convert, this saves seconds for large DBF files with many memo columns
	// as it avoids using the reflection in binary.Read
	hbuf := make([]byte, 8)
	_, err = handle.Read(hbuf)
	if err != nil {
		return nil, false, newError("dbase-io-readmemo-3", err)
	}
	sign := binary.BigEndian.Uint32(hbuf[:4])
	leng := binary.BigEndian.Uint32(hbuf[4:])
	debugf("Memo block header => text: %v, length: %d", sign == 1, leng)
	if leng == 0 {
		// No data according to block header? Not sure if this should be an error instead
		return []byte{}, sign == 1, nil
	}
	// Now read the actual data
	buf := make([]byte, leng)
	read, err := handle.Read(buf)
	if err != nil {
		return buf, false, newError("dbase-io-readmemo-4", err)
	}
	if read != int(leng) {
		return buf, sign == 1, newError("dbase-io-readmemo-5", ErrIncomplete)
	}
	if sign == 1 {
		buf, err = file.config.Converter.Decode(buf)
		if err != nil {
			return []byte{}, false, newError("dbase-io-readmemo-6", err)
		}
	}
	return buf, sign == 1, nil
}

// writeMemo writes a memo to the memo file and returns the address of the memo.
func (u UnixIO) WriteMemo(file *File, raw []byte, text bool, length int) ([]byte, error) {
	file.memoMutex.Lock()
	defer file.memoMutex.Unlock()
	if file.relatedHandle == nil {
		return nil, newError("dbase-io-writememo-1", ErrNoFPT)
	}
	handle, ok := file.relatedHandle.(*os.File)
	if !ok {
		return nil, newError("dbase-io-writememo-2", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	// Get the block position
	blockPosition := file.memoHeader.NextFree
	blocks := length / int(file.memoHeader.BlockSize)
	if length%int(file.memoHeader.BlockSize) > 0 {
		blocks++
	}
	// Write the memo header
	err := file.WriteMemoHeader(blocks)
	if err != nil {
		return nil, newError("dbase-io-writememo-2", err)
	}
	// Put the block data together
	data := make([]byte, 8)
	// The first 4 bytes are the signature, 1 for text, 0 for binary(image)
	if text {
		binary.BigEndian.PutUint32(data[:4], 1)
	} else {
		binary.BigEndian.PutUint32(data[:4], 0)
	}
	// The next 4 bytes are the length of the data
	binary.BigEndian.PutUint32(data[4:8], uint32(length))
	// The rest is the data
	data = append(data, raw...)
	// Lock the block we are writing to
	flock := &unix.Flock_t{
		Type:   unix.F_WRLCK,
		Start:  int64(blockPosition),
		Len:    int64(file.memoHeader.BlockSize),
		Whence: 0,
	}
	if file.config.WriteLock {
		for {
			err = unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EAGAIN) {
				return nil, newError("dbase-io-writememo-3", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			flock.Type = unix.F_ULOCK
			ulockErr := unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if ulockErr != nil {
				err = newError("dbase-io-writememo-4", ulockErr)
			}
		}()
	}
	position := int64(blockPosition) * int64(file.memoHeader.BlockSize)
	debugf("Writing memo block %d at position %d", blockPosition, position)
	// Seek to new the next free block
	_, err = handle.Seek(position, 0)
	if err != nil {
		return nil, newError("dbase-io-writememo-5", err)
	}
	// Write the memo data
	_, err = handle.Write(data)
	if err != nil {
		return nil, newError("dbase-io-writememo-6", err)
	}
	// Convert the block number to []byte
	address, err := toBinary(blockPosition)
	if err != nil {
		return nil, newError("dbase-io-writememo-7", err)
	}
	return address, nil
}

// writeMemoHeader writes the memo header to the memo file.
// Size is the number of blocks the new memo data will take up.
func (u UnixIO) WriteMemoHeader(file *File, size int) (err error) {
	if file.relatedHandle == nil {
		return newError("dbase-io-writememoheader-1", ErrNoFPT)
	}
	handle, ok := file.relatedHandle.(*os.File)
	if !ok {
		return newError("dbase-io-writememoheader-2", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	debugf("Writing memo header...")
	// Lock the block we are writing to
	if file.config.WriteLock {
		flock := &unix.Flock_t{
			Type:   unix.F_WRLCK,
			Start:  0,
			Len:    int64(file.header.FirstRow),
			Whence: 0,
		}
		for {
			err = unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if err == nil {
				break
			}

			if !errors.Is(err, unix.EAGAIN) {
				return newError("dbase-io-writememoheader-2", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			flock.Type = unix.F_ULOCK
			ulockErr := unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if ulockErr != nil {
				err = newError("dbase-io-writememoheader-3", ulockErr)
			}
		}()
	}
	// Seek to the beginning of the file
	_, err = handle.Seek(0, 0)
	if err != nil {
		return newError("dbase-io-writememoheader-4", err)
	}
	// Calculate the next free block
	file.memoHeader.NextFree += uint32(size)
	// Write the memo header
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[:4], file.memoHeader.NextFree)
	binary.BigEndian.PutUint16(buf[6:8], file.memoHeader.BlockSize)
	debugf("Writing memo header - next free: %d, block size: %d", file.memoHeader.NextFree, file.memoHeader.BlockSize)
	_, err = handle.Write(buf)
	if err != nil {
		return newError("dbase-io-writememoheader-5", err)
	}
	// Write null till end of header
	_, err = handle.Write(make([]byte, 512-8))
	if err != nil {
		return newError("dbase-io-writememoheader-6", err)
	}
	return nil
}

/**
 *	################################################################
 *	#				Row and Field IO handler
 *	################################################################
 */

// Reads raw row data of one row at rowPosition
func (u UnixIO) ReadRow(file *File, position uint32) ([]byte, error) {
	handle, ok := file.handle.(*os.File)
	if !ok {
		return nil, newError("dbase-io-readrow-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	if position >= file.header.RowsCount {
		return nil, newError("dbase-io-readrow-2", ErrEOF)
	}
	pos := int64(file.header.FirstRow) + (int64(position) * int64(file.header.RowLength))
	debugf("Reading row: %d at offset: %v", position, pos)
	buf := make([]byte, file.header.RowLength)
	_, err := handle.Seek(pos, 0)
	if err != nil {
		return buf, newError("dbase-io-readrow-3", err)
	}
	read, err := handle.Read(buf)
	if err != nil {
		return buf, newError("dbase-io-readrow-4", err)
	}
	if read != int(file.header.RowLength) {
		return buf, newError("dbase-io-readrow-5", ErrIncomplete)
	}
	return buf, nil
}

// writeRow writes raw row data to the given row position
func (u UnixIO) WriteRow(file *File, row *Row) (err error) {
	debugf("Writing row: %d ...", row.Position)
	row.handle.dbaseMutex.Lock()
	defer row.handle.dbaseMutex.Unlock()
	handle, ok := file.handle.(*os.File)
	if !ok {
		return newError("dbase-io-writerow-1", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	// Convert the row to raw bytes
	r, err := row.ToBytes()
	if err != nil {
		return newError("dbase-io-writerow-2", err)
	}
	// Update the header
	position := int64(row.handle.header.FirstRow) + (int64(row.Position) * int64(row.handle.header.RowLength))
	if row.Position >= row.handle.header.RowsCount {
		position = int64(row.handle.header.FirstRow) + (int64(row.Position-1) * int64(row.handle.header.RowLength))
		row.handle.header.RowsCount++
	}
	err = row.handle.WriteHeader()
	if err != nil {
		return newError("dbase-io-writerow-3", err)
	}
	// Lock the block we are writing to
	if row.handle.config.WriteLock {
		flock := &unix.Flock_t{
			Type:   unix.F_WRLCK,
			Start:  position,
			Len:    int64(row.handle.header.RowLength),
			Whence: 0,
		}
		for {
			err = unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if err == nil {
				break
			}

			if !errors.Is(err, unix.EAGAIN) {
				return newError("dbase-io-writerow-4", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			flock.Type = unix.F_ULOCK
			ulockErr := unix.FcntlFlock(handle.Fd(), unix.F_SETLK, flock)
			if ulockErr != nil {
				err = newError("dbase-io-writerow-5", ulockErr)
			}
		}()
	}
	debugf("Writing row: %d at offset: %v", row.Position, position)
	// Seek to the correct position
	_, err = handle.Seek(position, 0)
	if err != nil {
		return newError("dbase-io-writerow-5", err)
	}
	// Write the row
	_, err = handle.Write(r)
	if err != nil {
		return newError("dbase-io-writerow-6", err)
	}
	return nil
}

/**
 *	################################################################
 *	#						Search
 *	################################################################
 */

// Search searches for a row with the given value in the given field
func (u UnixIO) Search(file *File, field *Field, exactMatch bool) ([]*Row, error) {
	if field.column.DataType == 'M' {
		return nil, newError("dbase-io-search-1", fmt.Errorf("searching memo fields is not supported"))
	}
	handle, ok := file.handle.(*os.File)
	if !ok {
		return nil, newError("dbase-io-search-2", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	debugf("Searching for value: %v in field: %s", field.GetValue(), field.column.Name())
	// convert the value to a string
	val, err := file.GetRepresentation(field, !exactMatch)
	if err != nil {
		return nil, newError("dbase-io-search-3", err)
	}
	// Search for the value
	rows := make([]*Row, 0)
	position := uint64(file.header.FirstRow)
	for i := uint32(0); i < file.header.RowsCount; i++ {
		// Read the field value
		p := int64(position) + int64(field.column.Position)
		debugf("Searching at position: %d", p)
		_, err := handle.Seek(p, 0)
		position += uint64(file.header.RowLength)
		if err != nil {
			continue
		}
		buf := make([]byte, field.column.Length)
		read, err := handle.Read(buf)
		if err != nil {
			continue
		}
		if read != int(field.column.Length) {
			continue
		}
		// Check if the value matches
		if bytes.Contains(buf, val) {
			debugf("Found matching row %v at position: %d", i, p-int64(field.column.Position))
			err := file.GoTo(i)
			if err != nil {
				continue
			}
			row, err := file.Row()
			if err != nil {
				continue
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

/**
 *	################################################################
 *	#				General DBF handler
 *	################################################################
 */

// GoTo sets the internal row pointer to row rowNumber
// Returns and EOF error if at EOF and positions the pointer at lastRow+1
func (u UnixIO) GoTo(file *File, row uint32) error {
	if row > file.header.RowsCount {
		file.table.rowPointer = file.header.RowsCount
		return newError("dbase-io-goto-1", fmt.Errorf("%w, go to %v > %v", ErrEOF, row, file.header.RowsCount))
	}
	debugf("Going to row: %d", row)
	file.table.rowPointer = row
	return nil
}

// Skip adds offset to the internal row pointer
// If at end of file positions the pointer at lastRow+1
// If the row pointer would be become negative positions the pointer at 0
// Does not skip deleted rows
func (u UnixIO) Skip(file *File, offset int64) {
	newval := int64(file.table.rowPointer) + offset
	if newval >= int64(file.header.RowsCount) {
		file.table.rowPointer = file.header.RowsCount
	}
	if newval < 0 {
		file.table.rowPointer = 0
	}
	file.table.rowPointer = uint32(newval)
	debugf("Skipping %d row/s, new position: %d", offset, file.table.rowPointer)
}

// Returns if the row at internal row pointer is deleted
func (u UnixIO) Deleted(file *File) (bool, error) {
	if file.table.rowPointer >= file.header.RowsCount {
		return false, newError("dbase-io-deleted-1", ErrEOF)
	}
	handle, ok := file.handle.(*os.File)
	if !ok {
		return false, newError("dbase-io-deleted-2", fmt.Errorf("handle is of wrong type %T expected *os.File", file.handle))
	}
	_, err := handle.Seek(int64(file.header.FirstRow)+(int64(file.table.rowPointer)*int64(file.header.RowLength)), 0)
	if err != nil {
		return false, newError("dbase-io-deleted-2", err)
	}
	buf := make([]byte, 1)
	read, err := handle.Read(buf)
	if err != nil {
		return false, newError("dbase-io-deleted-3", err)
	}
	if read != 1 {
		return false, newError("dbase-io-deleted-4", ErrIncomplete)
	}
	return buf[0] == byte(Deleted), nil
}

func _findFile(name string) (string, error) {
	debugf("Searching for file: %s", name)
	// Read all files in the directory
	files, err := os.ReadDir(filepath.Dir(name))
	if err != nil {
		return "", newError("dbase-io-findfile-1", err)
	}
	for _, file := range files {
		if strings.EqualFold(file.Name(), filepath.Base(name)) {
			debugf("Found file: %s", file.Name())
			return filepath.Join(filepath.Dir(name), file.Name()), nil
		}
	}
	return name, nil
}
