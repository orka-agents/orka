package toolbox

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	elfHeaderBytes   = 20
	elfMachineAMD64  = 0x3e
	elfMachineAARCH6 = 0xb7
)

// elfArch reports whether the first bytes of a file are an ELF header and, if
// so, which supported architecture the e_machine field names. Unknown
// machines return an empty arch with isELF true so the caller can reject them.
func elfArch(r io.Reader) (arch string, isELF bool, err error) {
	var header [elfHeaderBytes]byte
	n, readErr := io.ReadFull(r, header[:])
	if readErr != nil {
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			_ = n
			return "", false, nil
		}
		return "", false, readErr
	}
	if header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F' {
		return "", false, nil
	}
	var order binary.ByteOrder = binary.LittleEndian
	if header[5] == 2 {
		order = binary.BigEndian
	}
	machine := order.Uint16(header[18:20])
	switch machine {
	case elfMachineAMD64:
		return "amd64", true, nil
	case elfMachineAARCH6:
		return "arm64", true, nil
	default:
		return "", true, nil
	}
}
