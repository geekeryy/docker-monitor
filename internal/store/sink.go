package store

import "reflect"

func isNilBatchSink(sink batchSink) bool {
	if sink == nil {
		return true
	}

	value := reflect.ValueOf(sink)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
