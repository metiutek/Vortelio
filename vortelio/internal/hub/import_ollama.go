package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ollama import — shared by the CLI (`vortelio import-ollama`) and the HTTP
// handler (/api/import/ollama), so importing works with or without a running
// server.
//
// Ollama stores manifests at ~/.ollama/models/manifests/<registry>/<ns>/<name>/<tag>
// and blobs at ~/.ollama/models/blobs/sha256-<digest>. We register them in
// Vortelio's manifest store WITHOUT copying — local_path points to the existing
// Ollama blob, so disk usage doesn't double.
// ─────────────────────────────────────────────────────────────────────────────

type ollamaManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Layers        []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// OllamaImportItem is one imported or skipped model.
type OllamaImportItem struct {
	Model  string `json:"model"`
	Size   int64  `json:"size,omitempty"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// OllamaImportResult is the outcome of an import run.
type OllamaImportResult struct {
	OllamaPath string             `json:"ollama_path"`
	DryRun     bool               `json:"dry_run"`
	Imported   []OllamaImportItem `json:"imported"`
	Skipped    []OllamaImportItem `json:"skipped"`
	Count      int                `json:"count"`
}

// OllamaDefaultDir returns the Ollama root (~/.ollama or from OLLAMA_MODELS).
func OllamaDefaultDir() string {
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		return filepath.Dir(filepath.Dir(v)) // OLLAMA_MODELS points to .../models
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ollama")
}

// ImportOllama scans an Ollama installation and registers its models in
// Vortelio's store (blobs are referenced in place, not copied). If root is
// empty, the default Ollama directory is used. On dryRun nothing is saved.
func ImportOllama(root string, dryRun bool) (OllamaImportResult, error) {
	if root == "" {
		root = OllamaDefaultDir()
	}
	manifestsDir := filepath.Join(root, "models", "manifests")
	blobsDir := filepath.Join(root, "models", "blobs")

	res := OllamaImportResult{OllamaPath: root, DryRun: dryRun}

	if _, err := os.Stat(manifestsDir); err != nil {
		return res, fmt.Errorf("Ollama installation not found at %s", root)
	}

	store := NewModelStore()

	filepath.WalkDir(manifestsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// path looks like: .../manifests/registry.ollama.ai/library/llama3/8b
		// The tag is the filename, the model is the parent dir.
		rel, _ := filepath.Rel(manifestsDir, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return nil
		}
		tag := parts[len(parts)-1]
		name := parts[len(parts)-2]

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var mf ollamaManifest
		if err := json.Unmarshal(data, &mf); err != nil {
			return nil
		}

		// Find the largest blob — that's the GGUF model file.
		var modelDigest string
		var modelSize int64
		var mmprojDigest string
		for _, l := range mf.Layers {
			if strings.Contains(l.MediaType, "model") && l.Size > modelSize {
				modelDigest = l.Digest
				modelSize = l.Size
			}
			if strings.Contains(l.MediaType, "projector") || strings.Contains(l.MediaType, "mmproj") {
				mmprojDigest = l.Digest
			}
		}
		if modelDigest == "" {
			res.Skipped = append(res.Skipped, OllamaImportItem{Model: name + ":" + tag, Reason: "no model layer"})
			return nil
		}

		blobName := strings.ReplaceAll(modelDigest, ":", "-")
		blobPath := filepath.Join(blobsDir, blobName)
		if _, err := os.Stat(blobPath); err != nil {
			res.Skipped = append(res.Skipped, OllamaImportItem{Model: name + ":" + tag, Reason: "blob missing: " + blobName})
			return nil
		}

		ref := &ModelRef{Type: "llm", Name: name, Tag: tag}
		if existing, _ := store.Resolve(ref); existing != nil {
			res.Skipped = append(res.Skipped, OllamaImportItem{Model: name + ":" + tag, Reason: "already installed"})
			return nil
		}

		item := OllamaImportItem{
			Model: fmt.Sprintf("llm/%s:%s", name, tag),
			Size:  modelSize,
			Path:  blobPath,
		}
		if dryRun {
			res.Imported = append(res.Imported, item)
			return nil
		}

		m := &Model{
			Type:         "llm",
			Name:         name,
			Tag:          tag,
			Format:       "gguf",
			SizeBytes:    modelSize,
			LocalPath:    blobPath,
			Source:       "ollama-import:" + root,
			Capabilities: []string{"chat", "completion"},
			DownloadedAt: time.Now(),
		}
		if mmprojDigest != "" {
			mmName := strings.ReplaceAll(mmprojDigest, ":", "-")
			mmPath := filepath.Join(blobsDir, mmName)
			if _, err := os.Stat(mmPath); err == nil {
				m.MmProjPath = mmPath
				m.Capabilities = append(m.Capabilities, "vision")
			}
		}
		if err := store.Save(m); err != nil {
			res.Skipped = append(res.Skipped, OllamaImportItem{Model: name + ":" + tag, Reason: "save failed: " + err.Error()})
			return nil
		}
		res.Imported = append(res.Imported, item)
		return nil
	})

	res.Count = len(res.Imported)
	return res, nil
}
