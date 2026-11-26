package flow

// maestro-d additions. This file is not part of upstream maestro-runner;
// it only re-exports parser internals the daemon needs so that no upstream
// file has to change. See docs/daemon/UPSTREAM.md.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// IsStepType reports whether key names a known YAML command (`tapOn`,
// `assertVisible`, …). The daemon uses it to reject unknown one-shot commands
// on the client side before anything reaches a device.
func IsStepType(key string) bool {
	return isStepType(key)
}

// StepTypes lists every known command name in a stable (source) order. It is
// derived from the same table isStepType checks, so a command added upstream
// appears here without further changes.
func StepTypes() []StepType {
	out := make([]StepType, 0, len(stepTypeOrder))
	for _, t := range stepTypeOrder {
		if isStepType(string(t)) {
			out = append(out, t)
		}
	}
	return out
}

// ParseStep parses one YAML (or JSON — JSON is valid YAML) step. The data is
// exactly what one list item of a flow file would contain: a bare command
// name (`waitForAnimationToEnd`), a mapping with the command as its key
// (`{"tapOn": {"id": "x"}}`), or a mapping holding the command plus optional
// BaseStep fields.
func ParseStep(data []byte, sourcePath string) (Step, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, &ParseError{Path: sourcePath, Message: fmt.Sprintf("invalid step: %v", err)}
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, &ParseError{Path: sourcePath, Message: "empty step"}
		}
		node = *node.Content[0]
	}
	return parseStep(&node, sourcePath)
}

// ParseSteps parses a YAML/JSON list of steps — the second document of a flow
// file without the config header.
func ParseSteps(data []byte, sourcePath string) ([]Step, error) {
	f := &Flow{SourcePath: sourcePath}
	if err := parseSteps(string(data), f); err != nil {
		return nil, err
	}
	return f.Steps, nil
}

// BuildStep builds a step from a command name and its YAML value (already
// decoded into Go data: string, map, slice, nil). This is what the one-shot CLI
// and the REST `/commands/{name}` route use: the value is re-encoded as YAML
// and handed to the upstream parser so every command keeps its exact YAML
// semantics (scalar shorthand, nested selectors, …).
func BuildStep(name string, value any, sourcePath string) (Step, error) {
	if !isStepType(name) {
		return nil, &ParseError{Path: sourcePath, Message: fmt.Sprintf("unknown step type: %s", name)}
	}
	var doc any
	if value == nil {
		doc = name
	} else {
		doc = map[string]any{name: value}
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, &ParseError{Path: sourcePath, Message: fmt.Sprintf("encode step: %v", err)}
	}
	return ParseStep(data, sourcePath)
}

// stepTypeOrder is the display order for StepTypes(): the same grouping the
// StepType constants use. Anything isStepType knows but this list omits is
// still accepted by the parser; it just won't be listed until added here.
var stepTypeOrder = []StepType{
	StepTapOn, StepDoubleTapOn, StepLongPressOn, StepTapOnPoint, StepDragAndDrop,
	StepSwipe, StepScroll, StepScrollUntilVisible, StepBack, StepHideKeyboard,
	StepOpenNotifications, StepAcceptAlert, StepDismissAlert,
	StepInputText, StepInputRandom, StepInputRandomEmail, StepInputRandomNumber,
	StepInputRandomPersonName, StepInputRandomText, StepEraseText, StepCopyTextFrom,
	StepPasteText, StepSetClipboard,
	StepAssertVisible, StepAssertNotVisible, StepAssertTrue, StepAssertCondition,
	StepAssertNoDefectsWithAI, StepAssertWithAI, StepExtractTextWithAI, StepWaitUntil,
	StepLaunchApp, StepStopApp, StepKillApp, StepClearState, StepClearKeychain, StepSetPermissions,
	StepSetLocation, StepSetOrientation, StepSetAirplaneMode, StepToggleAirplaneMode,
	StepSetDarkMode, StepToggleDarkMode, StepAssertDarkMode, StepAssertLightMode,
	StepTravel, StepOpenLink, StepOpenBrowser,
	StepRepeat, StepRetry, StepRunFlow, StepRunScript, StepRunShell, StepEvalScript,
	StepEvalBrowserScript, StepRunBrowserScript, StepEvalWebViewScript, StepRunWebViewScript,
	StepGetConsoleLogs, StepClearConsoleLogs, StepAssertNoJSErrors,
	StepSetCookies, StepGetCookies, StepSaveAuthState, StepLoadAuthState,
	StepUploadFile, StepWaitForDownload, StepGrantPermissions, StepResetPermissions,
	StepOpenTab, StepSwitchTab, StepCloseTab,
	StepMockNetwork, StepBlockNetwork, StepSetNetworkConditions, StepWaitForRequest, StepClearNetworkMocks,
	StepTakeScreenshot, StepAssertScreenshot, StepStartRecording, StepStopRecording, StepAddMedia, StepRemoveMedia,
	StepPressKey, StepWaitForAnimationToEnd, StepDefineVariables,
	StepSetTrafficShape, StepResetTrafficShape,
	StepSetStatusBar, StepKeyboardVisible, StepStartSampler, StepStopSampler,
	StepHoverOn, StepHoldOn, StepDragPath,
	StepMoveMouse, StepClickOn, StepScrollAt,
}
