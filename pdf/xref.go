package pdf

import (
	"fmt"
)

type XrefEntry struct {
	Offset int64
	Generation int64
	Type int64
}

func NewXrefEntry(offset int64, generation int64, type_value int64) *XrefEntry {
	return &XrefEntry{offset, generation, type_value}
}

func (entry *XrefEntry) String() string {
	return fmt.Sprintf("%d - %d - %d", entry.Type, entry.Offset, entry.Generation)
}
