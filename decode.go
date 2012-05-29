
/*
go-msgpack - Msgpack library for Go. Provides pack/unpack and net/rpc support.
https://github.com/ugorji/go-msgpack

Copyright (c) 2012, Ugorji Nwoke.
All rights reserved.

Redistribution and use in source and binary forms, with or without modification,
are permitted provided that the following conditions are met:

* Redistributions of source code must retain the above copyright notice,
  this list of conditions and the following disclaimer.
* Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.
* Neither the name of the author nor the names of its contributors may be used
  to endorse or promote products derived from this software
  without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR
ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON
ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package msgpack

// Code here is organized as follows:
// Exported methods are not called internally. They are just facades.
//   Unmarshal calls Decode 
//   Decode calls DecodeValue 
//   DecodeValue calls decodeValue 
// decodeValue and all other unexported functions use panics (not errors)
//    and may call other unexported functions (which use panics).

// Refactoring halted because we need a solution for map keys which are slices.
// Easy way is to convert it to a string.
// 
// To test with it, 
//   - change rpc.go, msgpack_test.go to use 
//     DecoderContainerResolver instead of *DecoderOptions (r:40, m:256, m:466)
//     &SimpleDecoderContainerResolver instead of &DecoderOptions (m: 467)

import (
	"io"
	"bytes"
	"reflect"
	"math"
	"fmt"
	"time"
	"encoding/binary"
)

// Some tagging information for error messages.
var (
	//_ = time.Parse
	msgTagDec = "msgpack.decoder"
	msgBadDesc = "Unrecognized descriptor byte: "
)

// Default DecoderContainerResolver used when a nil parameter is passed to NewDecoder().
// Sample Usage:
//   opts := msgpack.DefaultDecoderContainerResolver // makes a copy
//   opts.BytesStringLiteral = false // change some options
//   err := msgpack.NewDecoder(r, &opts).Decode(&v)
var DefaultDecoderContainerResolver = SimpleDecoderContainerResolver {
	MapType: nil,
	SliceType: nil,
	BytesStringLiteral: true,
	BytesStringSliceElement: true,
	BytesStringMapValue: true,
}

// A Decoder reads and decodes an object from an input stream in the msgpack format.
type Decoder struct {
	r io.Reader
	dam DecoderContainerResolver
	x [16]byte        //temp byte array re-used internally for efficiency
	t1, t2, t4, t8 []byte // use these, so no need to constantly re-slice
}

// DecoderContainerResolver has the DecoderContainer method for getting a usable reflect.Value
// when decoding a container (map, array, raw bytes) from a stream into a nil interface{}.
type DecoderContainerResolver interface {
	// DecoderContainer is used to get a proper reflect.Value when decoding 
	// a msgpack map, array or raw bytes (for which the stream defines the length and 
	// corresponding containerType) into a nil interface{}. 
	// 
	// This may be within the context of a container: ([]interface{} or map[XXX]interface{}),
	// or just a top-level literal.
	// 
	// The parentcontainer and parentkey define the context
	//   - If decoding into a map, they will be the map and the key in the map (a reflect.Value)
	//   - If decoding into a slice, they will be the slice and the index into the slice (an int)
	//   - Else they will be Invalid/nil
	// 
	// Custom code can use this callback to determine how specifically to decode something.
	// A simple implementation exists which just uses some options to do it 
	// (see SimpleDecoderContainerResolver).
	DecoderContainer(parentcontainer reflect.Value, parentkey interface{}, 
		length int, ct ContainerType) (val reflect.Value)
}

// DecoderContainerResolverFunc exposes a function as a DecoderContainerResolver
type DecoderContainerResolverFunc func(parentcontainer reflect.Value, parentkey interface{}, 
	length int, ct ContainerType) (val reflect.Value)

func (d DecoderContainerResolverFunc) DecoderContainer(
	parentcontainer reflect.Value, parentkey interface{}, length int, ct ContainerType) (val reflect.Value) {
	return d(parentcontainer, parentkey, length, ct)
}

// SimpleDecoderContainerResolver is a simple DecoderContainerResolver
// which uses some simple options to determine how to decode into a nil interface{}.
// Most applications will work fine with just this.
type SimpleDecoderContainerResolver struct {
	// If decoding into a nil interface{} and we detect a map in the stream,
	// we create a map of the type specified. It defaults to creating a 
	// map[interface{}]interface{} if not specified. 
	MapType reflect.Type
	// If decoding into a nil interface{} and we detect a slice/array in the stream,
	// we create a slice of the type specified. It defaults to creating a 
	// []interface{} if not specified. 
	SliceType reflect.Type
	// convert to a string if raw bytes are detected while decoding 
	// into a interface{},
	BytesStringLiteral bool
	// convert to a string if raw bytes are detected while decoding 
	// into a []interface{},
	BytesStringSliceElement bool
	// convert to a string if raw bytes are detected while decoding 
	// into a value in a map[XXX]interface{},
	BytesStringMapValue bool
}

// DecoderContainer supports common cases for decoding into a nil 
// interface{} depending on the context.
// 
// When decoding into a nil interface{}, the following rules apply as we have 
// to make assumptions about the specific types you want.
//    - Maps are decoded as map[interface{}]interface{} 
//      unless you provide a default map type when creating your decoder.
//      option: MapType
//    - Lists are always decoded as []interface{}
//      unless you provide a default slice type when creating your decoder.
//      option: SliceType
//    - raw bytes are decoded into []byte or string depending on setting of:
//      option: BytesStringMapValue     (if within a map value, use this setting)
//      option: BytesStringSliceElement (else if within a slice, use this setting)
//      option: BytesStringLiteral      (else use this setting)
func (d SimpleDecoderContainerResolver) DecoderContainer(
	parentcontainer reflect.Value, parentkey interface{}, 
	length int, ct ContainerType) (rvn reflect.Value) {
	switch ct {
	case ContainerMap:
		if d.MapType != nil {
			rvn = reflect.MakeMap(d.MapType)
		} else {
			rvn = reflect.MakeMap(mapIntfIntfTyp)
		}
	case ContainerList:
		if d.SliceType != nil {
			rvn = reflect.MakeSlice(d.SliceType, length, length)
		} else {
			rvn = reflect.MakeSlice(intfSliceTyp, length, length)
		}
	case ContainerRawBytes:
		rk := parentcontainer.Kind()
		if (rk == reflect.Invalid && d.BytesStringLiteral) ||
			(rk == reflect.Slice && d.BytesStringSliceElement) ||
			(rk == reflect.Map && d.BytesStringMapValue) {
			rvm := ""
			rvn = reflect.ValueOf(&rvm)
		} else {
			rvn = reflect.MakeSlice(byteSliceTyp, length, length)
		}
	}
	// fmt.Printf("DecoderContainer: %T, %v\n", rvn.Interface(), rvn.Interface())
	return
}

// NewDecoder returns a Decoder for decoding a stream of bytes into an object.
// If nil DecoderContainerResolver is passed, we use DefaultDecoderContainerResolver
func NewDecoder(r io.Reader, dam DecoderContainerResolver) (d *Decoder) {
	if dam == nil {
		dam = &DefaultDecoderContainerResolver
	}
	d = &Decoder{r:r, dam:dam}
	d.t1, d.t2, d.t4, d.t8 = d.x[:1], d.x[:2], d.x[:4], d.x[:8]
	return
}

// Decode decodes the stream from reader and stores the result in the 
// value pointed to by v.
// 
// If v is a pointer to a non-nil value, we will decode the stream into that value 
// (if the value type and the stream match. For example:
// integer in stream must go into int type (int8...int64), etc
// 
// If you do not know what type of stream it is, pass in a pointer to a nil interface.
// We will decode and store a value in that nil interface. 
// 
// time.Time is handled transparently, by (en)decoding (to)from a 
// []int64{Seconds since Epoch, Nanoseconds offset}.
// 
// Sample usages:
//   // Decoding into a non-nil typed value
//   var f float32
//   err = msgpack.NewDecoder(r, nil).Decode(&f)
//
//   // Decoding into nil interface
//   var v interface{}
//   dec := msgpack.NewDecoder(r, nil)
//   err = dec.Decode(&v)
//   
//   // To configure default options, see DefaultDecoderContainerResolver usage.
//   // or write your own DecoderContainerResolver
func (d *Decoder) Decode(v interface{}) (err error) {
	return d.DecodeValue(reflectValue(v))
}

// DecodeValue decodes the stream into a reflect.Value.
// The reflect.Value must be a pointer.
// See Decoder.Decode documentation. (Decode internally calls DecodeValue).
func (d *Decoder) DecodeValue(rv reflect.Value) (err error) {
	defer panicToErr(&err)
	// We cannot marshal into a non-pointer or a nil pointer 
	// (at least pass a nil interface so we can marshal into it)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		err = fmt.Errorf("%v: DecodeValue: Expecting valid pointer to decode into. Got: %v, %T, %v",
			msgTagDec, rv.Kind(), rv.Interface(), rv.Interface())
		return
	}

	//if a nil pointer is passed, set rv to the underlying value (not pointer).
	d.decodeValueT(0, -1, true, rv.Elem(), true, true, true)
	return
}

func (d *Decoder) decodeValueT(bd byte, containerLen int, readDesc bool, rve reflect.Value, 
	checkWasNilIntf bool, dereferencePtr bool, setToRealValue bool) (rvn reflect.Value) {
	rvn = rve
	wasNilIntf, rv := d.decodeValue(bd, containerLen, readDesc, rve)
	//if wasNilIntf, rv is either a pointer to actual value, a map or slice, or nil/invalid
	if ((checkWasNilIntf && wasNilIntf) || !checkWasNilIntf) && rv.IsValid() {
		if dereferencePtr && rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}
		if setToRealValue {
		   rve.Set(rv)
		}
		rvn = rv
	}
	return
}

func (d *Decoder) nilIntfDecode(bd0 byte, containerLen0 int, readDesc bool, setContainers bool, rv0 reflect.Value) (
	rv reflect.Value, bd byte, ct ContainerType, containerLen int, handled bool) {
	rv, bd, containerLen = rv0, bd0, containerLen0
	if readDesc {
		d.readb(1, d.t1)
		bd = d.t1[0]
	}
	//if we set the reflect.Value to an primitive value, consider it handled and return.
	handled = true
	switch {
	case bd == 0xc0:
	case bd == 0xc2:
		rv.Set(reflect.ValueOf(false))
	case bd == 0xc3:
		rv.Set(reflect.ValueOf(true))

	case bd == 0xca:
		rv.Set(reflect.ValueOf(math.Float32frombits(d.readUint32())))
	case bd == 0xcb:
		rv.Set(reflect.ValueOf(math.Float64frombits(d.readUint64())))
		
	case bd == 0xcc:
		rv.Set(reflect.ValueOf(d.readUint8()))
	case bd == 0xcd:
		rv.Set(reflect.ValueOf(d.readUint16()))
	case bd == 0xce:
		rv.Set(reflect.ValueOf(d.readUint32()))
	case bd == 0xcf:
		rv.Set(reflect.ValueOf(d.readUint64()))
		
	case bd == 0xd0:
		rv.Set(reflect.ValueOf(int8(d.readUint8())))
	case bd == 0xd1:
		rv.Set(reflect.ValueOf(int16(d.readUint16())))
	case bd == 0xd2:
		rv.Set(reflect.ValueOf(int32(d.readUint32())))
	case bd == 0xd3:
		rv.Set(reflect.ValueOf(int64(d.readUint64())))

	case bd == 0xda, bd == 0xdb, bd >= 0xa0 && bd <= 0xbf:
		ct = ContainerRawBytes
		if containerLen < 0 {
			containerLen = d.readContainerLen(bd, false, ct)
		}
		if setContainers {
			rv.Set(d.dam.DecoderContainer(reflect.Value{}, nil, containerLen, ct))
			rv = rv.Elem()
		}
		handled = false
	case bd == 0xdc, bd == 0xdd, bd >= 0x90 && bd <= 0x9f:
		ct = ContainerList
		if containerLen < 0 {
			containerLen = d.readContainerLen(bd, false, ct)
		}
		if setContainers {
			rv.Set(d.dam.DecoderContainer(reflect.Value{}, nil, containerLen, ct))
		}
		handled = false
	case bd == 0xde, bd == 0xdf, bd >= 0x80 && bd <= 0x8f:
		ct = ContainerMap
		if containerLen < 0 {
			containerLen = d.readContainerLen(bd, false, ct)
		}
		if setContainers {
			rv.Set(d.dam.DecoderContainer(reflect.Value{}, nil, containerLen, ct))
		}
		handled = false
	case bd >= 0xe0 && bd <= 0xff, bd >= 0x00 && bd <= 0x7f:
		// FIXNUM
		rv.Set(reflect.ValueOf(int8(bd)))
	default:
		handled = false
		d.err("Nil-Deciphered DecodeValue: %s: hex: %x, dec: %d", msgBadDesc, bd, bd)
	}
	return
}

func (d *Decoder) decodeValue(bd byte, containerLen int, readDesc bool, 
	rv0 reflect.Value) (wasNilIntf bool, rv reflect.Value) {
	//log(".. enter decode: rv: %v, %T, %v", rv0, rv0.Interface(), rv0.Interface())
	//defer func() {
	//	log("..  exit decode: rv: %v, %T, %v", rv, rv.Interface(), rv.Interface())
	//}()
	
	rv = rv0
	if readDesc {
		d.readb(1, d.t1)
		bd = d.t1[0]
	}

	rk := rv.Kind()
	wasNilIntf = rk == reflect.Interface && rv.IsNil()

	//if nil interface, use some hieristics to set the nil interface to an 
	//appropriate value based on the first byte read (byte descriptor bd)
	if wasNilIntf {
		var handled bool
		rv, bd, _, containerLen, handled = d.nilIntfDecode(bd, containerLen, false, true, rv)
		if handled {
			return
		}
		rk = rv.Kind()
	}
	
	if bd == 0xc0 {
		rv.Set(reflect.Zero(rv.Type()))	
		//log("==   nil decode: rv: %v, %v", rv, rv.Interface())
		return
	}
	
	switch rk {
	case reflect.Ptr, reflect.Interface:
		rvelem := rv.Elem()
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
			rvelem = rv.Elem()
		}
		d.decodeValue(bd, containerLen, false, rvelem)
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		switch bd {
		case 0xc2:
			rv.SetBool(false)
		case 0xc3:
			rv.SetBool(true)
			
		case 0xca:
			rv.SetFloat(float64(math.Float32frombits(d.readUint32())))
		case 0xcb:
			rv.SetFloat(math.Float64frombits(d.readUint64()))
			
		case 0xcc:
			rv.SetUint(uint64(d.readUint8()))
		case 0xcd:
			rv.SetUint(uint64(d.readUint16()))
		case 0xce:
			rv.SetUint(uint64(d.readUint32()))
		case 0xcf:
			rv.SetUint(d.readUint64())
			
		case 0xd0:
			rv.SetInt(int64(int8(d.readUint8())))
		case 0xd1:
			rv.SetInt(int64(int16(d.readUint16())))
		case 0xd2:
			rv.SetInt(int64(int32(d.readUint32())))
		case 0xd3:
			rv.SetInt(int64(d.readUint64()))

		default:
			//may be a single-byte integer
			defHandled := false
			switch rk {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				// if sbd > -32 && sbd <= math.MaxInt8 {
				if bd >= 0x00 && bd <= 0x7f || bd >= 0xe0 && bd <= 0xff {
					rv.SetInt(int64(int8(bd)))
					defHandled = true
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				// if bd <= math.MaxInt8 {
				if bd >= 0x00 && bd <= 0x7f {
					rv.SetUint(uint64(bd))
					defHandled = true
				}
			}
			if !defHandled {
				d.err("DefNotHandled DecodeValue: %s: %x", msgBadDesc, bd)
			}
		}
	case reflect.Slice, reflect.Array, reflect.String:
		rvtype := rv.Type()
		isString := rk == reflect.String
		isByteSlice := rvtype == byteSliceTyp
		rawbytes := isString || isByteSlice
		
		if containerLen < 0 {
			if rawbytes {
				containerLen = d.readContainerLen(bd, false, ContainerRawBytes)
			} else {
				containerLen = d.readContainerLen(bd, false, ContainerList)
			} 
		}
		if containerLen == 0 {
			break
		}
		
		if rawbytes {
			var bs []byte
			if isByteSlice {
				bs = rv.Bytes()
			}
			if len(bs) < containerLen {
				bs = make([]byte, containerLen)
				if isByteSlice {
					rv.Set(reflect.ValueOf(bs))
				}
			} else if len(bs) > containerLen {
				bs = bs[:containerLen]
			}
			d.readb(containerLen, bs)
			if isString {
				rv.SetString(string(bs))
			} 
			break
		}
		if isString {
			d.err("Strings must be handled as raw bytes")
		}
		rvelemtype := rvtype.Elem()

		rvlen := rv.Len()
		switch rk {
		case reflect.Array:
			if rvlen < containerLen {
				d.err("Array len: %d must be >= container Len: %d", rvlen, containerLen)
			} else if rvlen > containerLen {
				for j := containerLen; j < rvlen; j++ {
					rv.Index(j).Set(reflect.Zero(rvelemtype))
				}
			}
		case reflect.Slice:
			switch {
			case rv.IsNil():
				rv.Set(reflect.MakeSlice(rvtype, containerLen, containerLen))
			case containerLen > rvlen:
			switch {
				case containerLen > rv.Cap():
					rv2 := reflect.MakeSlice(rvtype, containerLen, containerLen)
					if rvlen > 0 {
						reflect.Copy(rv2, rv)
					}
					rv.Set(rv2)
				default:
					rv.SetLen(containerLen)
				}
			}
			rvlen = containerLen
		}
		
		for j := 0; j < containerLen; j++ {
			rvj := rv.Index(j)
			if rvelemtype == intfTyp && rvj.IsNil() {
				rvj, bd0, ct0, containerLen0, handled0 := d.nilIntfDecode(0, -1, true, false, rvj)
				// fmt.Printf("intfTyp: %v, %v, %v, %v, %v\n", rvj.Interface(), bd0, ct0, containerLen0, handled0)
				if !handled0 {
					if rvj2 := d.dam.DecoderContainer(rv, j, containerLen0, ct0); rvj2.IsValid() {
						rvj2 = d.decodeValueT(bd0, containerLen0, false, rvj2, false, true, false)
						rvj.Set(rvj2)
					} else {
						d.decodeValueT(bd0, containerLen0, false, rvj, true, true, true)
					}
				}
			} else {
				d.decodeValueT(0, -1, true, rvj, true, true, true)
			}
		}
	case reflect.Struct:
		rvtype := rv.Type()
		if rvtype == timeTyp {
			tt := [2]int64{}
			d.decodeValue(bd, -1, false, reflect.ValueOf(&tt).Elem())
			rv.Set(reflect.ValueOf(time.Unix(tt[0], tt[1]).UTC()))
			break
		}
		
		if containerLen < 0 {
			containerLen = d.readContainerLen(bd, false, ContainerMap)
		}
		if containerLen == 0 {
			break
		}
		for j := 0; j < containerLen; j++ {
			rvkencname := ""
			rvk := reflect.ValueOf(&rvkencname).Elem()
			d.decodeValue(0, -1, true, rvk)
			rvksi := getStructFieldInfos(rvtype).getForEncName(rvkencname)
			if rvksi == nil {
				d.err("DecodeValue: Invalid Enc Field: %s", rvkencname)
			}

			d.decodeValueT(0, -1, true, rvksi.field(rv), true, true, true)
		}
	case reflect.Map:
		if containerLen < 0 {
			containerLen = d.readContainerLen(bd, false, ContainerMap)
		}
		if containerLen == 0 {
			break
		}
		rvtype := rv.Type()
		ktype, vtype := rvtype.Key(), rvtype.Elem()			
		if rv.IsNil() {
			rvn := reflect.MakeMap(rvtype)
			rv.Set(rvn)
		}
		for j := 0; j < containerLen; j++ {
			rvk := reflect.New(ktype).Elem()
			rvk = d.decodeValueT(0, -1, true, rvk, true, true, false)
			
			if ktype == intfTyp && rvk.Type() == byteSliceTyp {
				rvk = reflect.ValueOf(string(rvk.Bytes()))
			}
			rvv := rv.MapIndex(rvk)
			if !rvv.IsValid() {
				rvv = reflect.New(vtype).Elem()
			}
			if vtype == intfTyp && rvv.IsNil() {
				rvv, bd0, ct0, containerLen0, handled0 := d.nilIntfDecode(0, -1, true, false, rvv)
				if !handled0 {
					if rvv2 := d.dam.DecoderContainer(rv, rvk, containerLen0, ct0); rvv2.IsValid() {
						rvv2 = d.decodeValueT(bd0, containerLen0, false, rvv2, false, true, false)
						rvv.Set(rvv2)
					} else {
						rvv = d.decodeValueT(bd0, containerLen0, false, rvv, true, true, false)
					}
				}
			} else {
				rvv = d.decodeValueT(0, -1, true, rvv, true, true, false)
			}
			rv.SetMapIndex(rvk, rvv)
		}
	}
	return
}

// read a number of bytes into bs, and return an appropriate
// []byte with length adjusted.
func (d *Decoder) readb(numbytes int, bs []byte) {
	n, err := d.r.Read(bs)
	if err != nil {
		d.err("Error: %v", err)
	} else if n != numbytes {
		//try to read one more time. This is necessary for example, if using a bufio.Reader,
		//where at end of buffer, only a subset is returned, and remaining got next time.
		n2, numbytes2 := 0, numbytes-n
		n2, err = d.r.Read(bs[n:])
		if err != nil {
			d.err("Error: %v", err)
		} else if n2 != numbytes2 {
			d.err("read: Incorrect num bytes read. Expecting: %v, Received: %v", numbytes, n+n2)
		}
	}
}

func (d *Decoder) readUint8() uint8 {
	d.readb(1, d.t1)
	return d.t1[0]
}

func (d *Decoder) readUint16() uint16 {
	d.readb(2, d.t2)
	return binary.BigEndian.Uint16(d.t2)
}

func (d *Decoder) readUint32() uint32 {
	d.readb(4, d.t4)
	return binary.BigEndian.Uint32(d.t4)
}

func (d *Decoder) readUint64() uint64 {
	d.readb(8, d.t8)
	return binary.BigEndian.Uint64(d.t8)
}

func (d *Decoder) readContainerLen(bd byte, readDesc bool, ct ContainerType) (l int) {
	// bd is the byte descriptor. First byte is always descriptive.
	if readDesc {
		d.readb(1, d.t1)
		bd = d.t1[0]
	}
	_, b0, b1, b2 := getContainerByteDesc(ct)

	switch {
	case bd == b1:
		l = int(d.readUint16())
	case bd == b2:
		l = int(d.readUint32())
	case (b0 & bd) == b0:
		l = int(b0 ^ bd)
	default:
		d.err("readContainerLen: %s: hex: %x, dec: %d", msgBadDesc, bd, bd)
	}
	return	
}

func (d *Decoder) err(format string, params ...interface{}) {
	doPanic(msgTagDec, format, params)
}

// Unmarshal is a convenience function which decodes a stream of bytes into v.
// It delegates to Decoder.Decode.
func Unmarshal(data []byte, v interface{}, dam DecoderContainerResolver) error {
	return NewDecoder(bytes.NewBuffer(data), dam).Decode(v)
}
