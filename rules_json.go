package main

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

func readJSON(ctx context.Context, d *json.Decoder, depth int) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := d.Token()
	if err != nil {
		return nil, ErrValidation
	}
	switch v := token.(type) {
	case json.Delim:
		if depth >= 64 {
			return nil, ErrValidation
		}
		switch v {
		case '{':
			object := map[string]any{}
			for d.More() {
				keyToken, e := d.Token()
				if e != nil {
					return nil, ErrValidation
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, ErrValidation
				}
				if _, exists := object[key]; exists {
					return nil, ErrValidation
				}
				item, e := readJSON(ctx, d, depth+1)
				if e != nil {
					return nil, e
				}
				object[key] = item
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, ErrValidation
			}
			return object, nil
		case '[':
			array := []any{}
			for d.More() {
				item, e := readJSON(ctx, d, depth+1)
				if e != nil {
					return nil, e
				}
				array = append(array, item)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, ErrValidation
			}
			return array, nil
		default:
			return nil, ErrValidation
		}
	case json.Number:
		if _, err := numberRat(v); err != nil {
			return nil, err
		}
		return v, nil
	case nil, string, bool:
		return v, nil
	default:
		return nil, ErrValidation
	}
}
func numberRat(n json.Number) (*big.Rat, error) {
	text := string(n)
	if len(text) > 256 {
		return nil, ErrValidation
	}
	exponent := 0
	if idx := strings.IndexAny(text, "eE"); idx >= 0 {
		v, err := strconv.Atoi(text[idx+1:])
		if err != nil || v > 1000 || v < -1000 {
			return nil, ErrValidation
		}
		exponent = v
		text = text[:idx]
	}
	if idx := strings.IndexByte(text, '.'); idx >= 0 {
		exponent -= len(text) - idx - 1
		text = text[:idx] + text[idx+1:]
	}
	numerator, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, ErrValidation
	}
	denominator := big.NewInt(1)
	if exponent != 0 {
		size := exponent
		if size < 0 {
			size = -size
		}
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(size)), nil)
		if exponent > 0 {
			numerator.Mul(numerator, power)
		} else {
			denominator = power
		}
	}
	return new(big.Rat).SetFrac(numerator, denominator), nil
}
func scalarEqual(a, b any) bool {
	switch expected := b.(type) {
	case nil:
		return a == nil
	case string:
		actual, ok := a.(string)
		return ok && actual == expected
	case bool:
		actual, ok := a.(bool)
		return ok && actual == expected
	case json.Number:
		actual, ok := a.(json.Number)
		if !ok {
			return false
		}
		x, e := numberRat(actual)
		y, f := numberRat(expected)
		return e == nil && f == nil && x.Cmp(y) == 0
	default:
		return false
	}
}
