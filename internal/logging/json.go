package logging

import "encoding/json"

func writeJSONLine(file *lineFile, value any) {
	line, err := json.Marshal(value)
	if err != nil {
		file.warns.Warnf("warning: failed to marshal %s log %s: %v\n", file.kind, file.path, err)
		return
	}
	file.WriteLine(line)
}
