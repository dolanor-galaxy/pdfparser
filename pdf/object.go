package pdf

import (
	"fmt"
)

type Object interface {
	String() string
}

type IndirectObject struct {
	Number int64
	Generation int64
	Value Object
	Stream []byte
}

func NewIndirectObject(number int64) *IndirectObject {
	return &IndirectObject{number, 0, NewTokenString("null"), nil}
}

func (obj *IndirectObject) Print() {
	fmt.Printf("%d %d obj\n", obj.Number, obj.Generation)
	fmt.Println(obj.Value.String())
	if obj.Stream != nil {
		fmt.Println("stream")
		fmt.Println(string(obj.Stream))
		fmt.Println("endstream")
	}
	fmt.Println("endobj")
}