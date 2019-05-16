package pdf

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

var whitespace = []byte("\x00\t\n\f\r ")
var delimiters = []byte("()<>[]/%")

type Tokenizer struct {
	*bufio.Reader
}

func NewTokenizer(reader io.Reader) *Tokenizer {
	return &Tokenizer{bufio.NewReader(reader)}
}

func (tokenizer *Tokenizer) NextToken() (*Token, error) {
	// skip leading whitespace
	b, err := tokenizer.SkipWhitespace()
	if err != nil {
		return nil, err
	}

	// start a new token
	token := NewToken(b)

	// if start or end of array then return as token
	if b == '[' || b == ']' {
		return token, nil
	}

	// if start of string
	if b == '(' {
		// find balanced closing bracket
		for open_parens := 1; open_parens > 0; {
			// read next byte
			b, err = tokenizer.ReadByte()
			if err != nil {
				return nil, err
			}

			// if this is the start of an escape sequence
			if b == '\\' {
				// read next byte
				b, err = tokenizer.ReadByte()
				if err != nil {
					return nil, err
				}

				// ignore line breaks \n or \r or \r\n
				if b == '\n' {
					continue
				}
				if b == '\r' {
					// read next byte
					b, err = tokenizer.ReadByte()
					if err != nil {
						return nil, err
					}
					// if byte is not a new line then unread it
					if b != '\n' {
						err = tokenizer.UnreadByte()
						if err != nil {
							return nil, err
						}
					}
					continue
				}

				// if this is the start of an octal character code
				if b >= '0' && b <= '7' {
					// add byte to character code
					code := bytes.NewBuffer([]byte{b})

					// add at most 2 more bytes to code
					for i := 0; i < 2; i++ {
						// read next byte
						b, err = tokenizer.ReadByte()
						if err != nil {
							return nil, err
						}

						// if next byte is not part of the octal code
						if b < '0' || b > '7' {
							// unread the byte and stop collecting code
							err = tokenizer.UnreadByte()
							if err != nil {
								return nil, err
							}
							break
						}

						// add byte to code
						code.WriteByte(b)
					}

					// convert code into byte
					val, err := strconv.ParseInt(string(code.Bytes()), 8, 16)
					if err != nil {
						return nil, err
					}
					b = byte(val)
				}

				// add byte to token and continue
				token.WriteByte(b)
				continue
			}

			// add byte to token
			token.WriteByte(b)

			// keep track of number of open parens
			if b == '(' {
				open_parens++
			} else if b == ')' {
				open_parens--
			}
		}

		// return string
		return token, nil
	}

	// if start of name
	if b == '/' {
		// parse name
		for {
			// read in the next byte
			b, err = tokenizer.ReadByte()
			if err != nil {
				return nil, err
			}

			// if the next byte is whitespace or delimiter then unread it and return the token
			if bytes.IndexByte(delimiters, b) >= 0 || bytes.IndexByte(whitespace, b) >= 0 {
				err = tokenizer.UnreadByte()
				if err != nil {
					return nil, err
				}
				return token, nil
			}

			// if next byte is the start of a hex character code
			if b == '#' {
				// read the next 2 bytes
				code, err := tokenizer.Peek(2)
				if err != nil {
					return nil, err
				}
				_, err = tokenizer.Discard(2)
				if err != nil {
					return nil, err
				}

				// convert the hex code to a byte
				val, err := strconv.ParseInt(string(code), 16, 16)
				if err != nil {
					return nil, err
				}
				b = byte(val)
			}

			// add byte to token
			token.WriteByte(b)
		}
	}

	// if start of hex string or dictionary
	if b == '<' {
		// get next byte
		b, err = tokenizer.SkipWhitespace()
		if err != nil {
			return nil, err
		}

		// if this is the dictionary start marker then return token
		if b == '<' {
			token.WriteByte(b)
			return token, nil
		}

		for {
			// get next byte
			b2, err := tokenizer.SkipWhitespace()
			if err != nil {
				return nil, err
			}

			// early end of hex string last character is assumed to be 0
			if b2 == '>' {
				// add decoded byte to token
				v, err := strconv.ParseUint(string([]byte{b, '0'}), 16, 8)
				if err != nil {
					return nil, err
				}
				token.WriteByte(byte(v))

				// add terminating marker to token and return
				token.WriteByte('>')
				return token, nil
			}

			// add decoded byte to token
			v, err := strconv.ParseUint(string([]byte{b, b2}), 16, 8)
			if err != nil {
				return nil, err
			}
			token.WriteByte(byte(v))

			// get next byte
			b, err = tokenizer.SkipWhitespace()
			if err != nil {
				return nil, err
			}

			// if end of hex string
			if b == '>' {
				// add terminating marker to token and return
				token.WriteByte('>')
				return token, nil
			}
		}
	}

	// if end of dictionary
	if b == '>' {
		// get the next byte
		b, err = tokenizer.ReadByte()
		if err != nil {
			return nil, err
		}
		token.WriteByte(b)

		// confirm token is a dictionary end
		if b != '>' {
			return nil, errors.New(fmt.Sprintf("Expected > instead of %b", b))
		}
		return token, nil
	}

	// set token is number if first byte is a digit
	token.IsNumber = b >= '0' && b <= '9'

	// ordinary token, scan until next whitespace or delimiter
	for {
		// get next byte
		b, err = tokenizer.ReadByte()
		if err != nil {
			return nil, err
		}

		// if byte is whitespace or delimiter then unread byte and return token
		if bytes.IndexByte(whitespace, b) >= 0 || bytes.IndexByte(delimiters, b) >= 0 {
			err = tokenizer.UnreadByte()
			if err != nil {
				return nil, err
			}
			return token, nil
		}

		// update is number
		token.IsNumber = token.IsNumber && b >= '0' && b <= '9'

		// add byte to token
		token.WriteByte(b)
	}
}

func (tokenizer *Tokenizer) SkipWhitespace() (byte, error) {
	for {
		// get next byte
		b, err := tokenizer.ReadByte()
		if err != nil {
			return 0, err
		}

		// advance if next byte is whitespace
		if bytes.IndexByte(whitespace, b) >= 0 {
			continue
		}

		// if next byte is start of a comment then advance until next line
		if b == '%' {
			_, err = tokenizer.ReadBytes('%')
			if err != nil {
				return 0, err
			}
			continue
		}

		// next byte is neither comment or whitespace so return
		return b, nil
	}
}
