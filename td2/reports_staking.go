package tenderduty

import (
	"fmt"
	"math"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
)

func parseReportDelegators(b []byte) (*uint64, error) {
	var count *uint64
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 2 || typ != protowire.BytesType {
			return nil
		}
		n := uint64(0)
		count = &n
		return eachField(value, func(num protowire.Number, typ protowire.Type, _ []byte, v uint64) error {
			if num == 2 && typ == protowire.VarintType {
				*count = v
			}
			return nil
		})
	})
	if err == nil && count == nil {
		err = fmt.Errorf("delegator pagination unavailable")
	}
	if err != nil {
		return nil, err
	}
	return count, nil
}

func parseReportSelf(b []byte) (*float64, error) {
	var amount *float64
	err := eachField(b, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
			if num != 2 || typ != protowire.BytesType {
				return nil
			}
			return eachField(value, func(num protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
				if num == 2 && typ == protowire.BytesType {
					n, err := strconv.ParseFloat(string(value), 64)
					if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
						return fmt.Errorf("invalid self bond amount")
					}
					amount = &n
				}
				return nil
			})
		})
	})
	if err == nil && amount == nil {
		err = fmt.Errorf("self bond unavailable")
	}
	if err != nil {
		return nil, err
	}
	return amount, nil
}
