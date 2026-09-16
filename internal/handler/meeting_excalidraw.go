package handler

import (
	"bytes"
	"encoding/json"
)

// mergeExcalidrawScene applies an optimistic, versioned element delta to the
// authoritative room snapshot. The room mutex serializes the commit, while
// element versions allow two different users to draw concurrently without a
// last-writer-wins replacement of the entire scene.
func mergeExcalidrawScene(previous, incoming []byte, delta bool) ([]byte, bool, error) {
	incomingRoot, err := decodeExcalidrawObject(incoming)
	if err != nil {
		return nil, false, err
	}
	delete(incomingRoot, "delta")
	if !delta || len(bytes.TrimSpace(previous)) == 0 {
		previousRoot, previousErr := decodeExcalidrawObject(previous)
		if previousErr == nil {
			if _, hasFiles := incomingRoot["files"]; !hasFiles {
				if previousFiles, ok := previousRoot["files"]; ok {
					incomingRoot["files"] = previousFiles
				}
			}
		}
		merged, marshalErr := json.Marshal(incomingRoot)
		if marshalErr != nil {
			return nil, false, marshalErr
		}
		return merged, !bytes.Equal(bytes.TrimSpace(previous), merged), nil
	}

	previousRoot, err := decodeExcalidrawObject(previous)
	if err != nil {
		return nil, false, err
	}
	previousElements, err := decodeExcalidrawElements(previousRoot["elements"])
	if err != nil {
		return nil, false, err
	}
	incomingElements, err := decodeExcalidrawElements(incomingRoot["elements"])
	if err != nil {
		return nil, false, err
	}

	mergedElements := make([]map[string]json.RawMessage, len(previousElements))
	indices := make(map[string]int, len(previousElements))
	for index, element := range previousElements {
		mergedElements[index] = element
		if id := rawExcalidrawString(element["id"]); id != "" {
			indices[id] = index
		}
	}
	changed := false
	for _, incomingElement := range incomingElements {
		id := rawExcalidrawString(incomingElement["id"])
		if id == "" {
			continue
		}
		index, exists := indices[id]
		if !exists {
			indices[id] = len(mergedElements)
			mergedElements = append(mergedElements, incomingElement)
			changed = true
			continue
		}
		merged, accepted, mergeErr := mergeExcalidrawElement(mergedElements[index], incomingElement)
		if mergeErr != nil {
			return nil, false, mergeErr
		}
		if accepted {
			mergedElements[index] = merged
			changed = true
		}
	}

	mergedElementsRaw, err := json.Marshal(mergedElements)
	if err != nil {
		return nil, false, err
	}
	previousRoot["elements"] = mergedElementsRaw
	if incomingFiles, ok := incomingRoot["files"]; ok {
		mergedFiles, fileChanged, fileErr := mergeExcalidrawFiles(previousRoot["files"], incomingFiles)
		if fileErr != nil {
			return nil, false, fileErr
		}
		if fileChanged {
			previousRoot["files"] = mergedFiles
			changed = true
		}
	}
	mergedScene, err := json.Marshal(previousRoot)
	if err != nil {
		return nil, false, err
	}
	return mergedScene, changed, nil
}

func decodeExcalidrawObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return make(map[string]json.RawMessage), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		object = make(map[string]json.RawMessage)
	}
	return object, nil
}

func decodeExcalidrawElements(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []map[string]json.RawMessage{}, nil
	}
	var elements []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, err
	}
	return elements, nil
}

func rawExcalidrawString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func rawExcalidrawNumber(raw json.RawMessage) int64 {
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	return value
}

func excalidrawElementWins(incoming, current map[string]json.RawMessage) bool {
	incomingVersion := rawExcalidrawNumber(incoming["version"])
	currentVersion := rawExcalidrawNumber(current["version"])
	if incomingVersion != currentVersion {
		return incomingVersion > currentVersion
	}
	incomingNonce := rawExcalidrawNumber(incoming["versionNonce"])
	currentNonce := rawExcalidrawNumber(current["versionNonce"])
	if incomingNonce != 0 || currentNonce != 0 {
		return incomingNonce > currentNonce
	}
	return false
}

func mergeExcalidrawElement(current, incoming map[string]json.RawMessage) (map[string]json.RawMessage, bool, error) {
	pointsAppend, hasAppend := incoming["pointsAppend"]
	pointsBase, hasBase := incoming["pointsBase"]
	appendIsNextTail := false
	if hasAppend {
		var appended []json.RawMessage
		if err := json.Unmarshal(pointsAppend, &appended); err != nil {
			return nil, false, err
		}
		var existing []json.RawMessage
		if err := json.Unmarshal(current["points"], &existing); err == nil {
			baseLength := len(existing)
			if hasBase {
				var declared int
				if err := json.Unmarshal(pointsBase, &declared); err != nil {
					return nil, false, err
				}
				appendIsNextTail = declared == baseLength
			} else {
				appendIsNextTail = rawExcalidrawNumber(incoming["version"]) > rawExcalidrawNumber(current["version"])
			}
		}
	}
	if hasAppend && !appendIsNextTail {
		return current, false, nil
	}
	if !excalidrawElementWins(incoming, current) && !appendIsNextTail {
		return current, false, nil
	}
	merged := make(map[string]json.RawMessage, len(current)+len(incoming))
	for key, value := range current {
		merged[key] = value
	}
	delete(incoming, "pointsAppend")
	delete(incoming, "pointsBase")
	for key, value := range incoming {
		merged[key] = value
	}
	if hasAppend && appendIsNextTail {
		var appended []json.RawMessage
		if err := json.Unmarshal(pointsAppend, &appended); err != nil {
			return nil, false, err
		}
		var existing []json.RawMessage
		if err := json.Unmarshal(current["points"], &existing); err != nil {
			return merged, true, nil
		}
		existing = append(existing, appended...)
		encoded, err := json.Marshal(existing)
		if err != nil {
			return nil, false, err
		}
		merged["points"] = encoded
	}
	return merged, true, nil
}

func mergeExcalidrawFiles(previous, incoming json.RawMessage) (json.RawMessage, bool, error) {
	previousFiles, err := decodeExcalidrawObject(previous)
	if err != nil {
		return nil, false, err
	}
	incomingFiles, err := decodeExcalidrawObject(incoming)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for id, file := range incomingFiles {
		if !bytes.Equal(previousFiles[id], file) {
			previousFiles[id] = file
			changed = true
		}
	}
	merged, err := json.Marshal(previousFiles)
	return merged, changed, err
}
