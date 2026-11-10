package llm

import (
	"bytes"
	"fmt"
	"text/template"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// ValidatePromptTemplate checks that a prompt template not only parses but
// also executes against representative datasets for its feature. Chat prompt
// data carries two booleans (AllowTeasing, ImageRecognitionEnabled), so all
// four combinations execute — catching branch-conditional defects such as
// {{if .AllowTeasing}}…{{else}}{{.Bogus}}{{end}} that parse-only validation
// misses. Summarize and image-recognition data has no booleans, so a single
// dataset covers them.
//
// Scope of the guarantee: execution against representative-shaped data catches
// unknown or misused placeholders, but it cannot prove an arbitrary template
// executes for every conceivable runtime value. Render-time failures keep
// surfacing as regular execution errors exactly as before.
func ValidatePromptTemplate(feature adminconfig.PromptFeature, body string) error {
	datasets, err := promptValidationDatasets(feature)
	if err != nil {
		return err
	}

	tmpl, err := template.New(string(feature)).Parse(body)
	if err != nil {
		return fmt.Errorf("parse template %s: %w", feature, err)
	}

	for _, dataset := range datasets {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, dataset.data); err != nil {
			return fmt.Errorf("check template %s failed (%s): %w", feature, dataset.label, err)
		}
	}

	return nil
}

type promptValidationDataset struct {
	label string
	data  any
}

func promptValidationDatasets(feature adminconfig.PromptFeature) ([]promptValidationDataset, error) {
	switch feature {
	case adminconfig.PromptFeatureChat:
		datasets := make([]promptValidationDataset, 0, 4)
		for _, allowTeasing := range []bool{false, true} {
			for _, imageRecognition := range []bool{false, true} {
				datasets = append(datasets, promptValidationDataset{
					label: fmt.Sprintf("allow_teasing=%t, image_recognition_enabled=%t", allowTeasing, imageRecognition),
					data: ChatPromptData{
						Language:                "English",
						Model:                   "test-model",
						Context:                 "Recent conversation context.",
						Gender:                  "neutral",
						CharacterName:           "Kitsune",
						ToneMode:                "playful",
						AllowTeasing:            allowTeasing,
						ImageRecognitionEnabled: imageRecognition,
						ToolPolicy:              "Tools: image recognition enabled.",
					},
				})
			}
		}

		return datasets, nil
	case adminconfig.PromptFeatureSummarize:
		return []promptValidationDataset{{
			label: "summarize",
			data:  SummarizePromptData{Language: "English", MaxLength: 2000},
		}}, nil
	case adminconfig.PromptFeatureImageRecognition:
		return []promptValidationDataset{{
			label: "image_recognition",
			data:  ImageRecognitionPromptData{Language: "English"},
		}}, nil
	default:
		return nil, fmt.Errorf("unknown prompt feature %q", feature)
	}
}
