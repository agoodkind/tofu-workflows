package input

import (
	"errors"

	strictjson "sigs.k8s.io/json"
)

// UnmarshalStringMap rejects duplicate keys and non-string values.
func UnmarshalStringMap(data []byte) (map[string]string, error) {
	var values map[string]string
	strictErrors, decodeErr := strictjson.UnmarshalStrict(data, &values)
	if err := errors.Join(append(strictErrors, decodeErr)...); err != nil {
		return nil, errors.New(
			"bundle must contain a JSON object with unique keys and string values",
		)
	}

	return values, nil
}
