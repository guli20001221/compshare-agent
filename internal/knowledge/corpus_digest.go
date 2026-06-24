package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// CorpusDigestExpected pins deploy/kb/stage2b_w0.jsonl (LF-normalized SHA256).
// Bumped 2026-06-10 for the A800-无卡 factual hotfix (A800 removed from the
// 无卡开机 supported-card lists across 5 chunks; price/inventory/Spot mentions of
// A800 left intact). The qwen3 sidecar was renamed to this digest but NOT
// re-embedded: vectors are keyed by stable chunk_id (bijection holds), and the
// 5 edited chunks keep their prior-text vectors. That is acceptable here — the
// edit is a tiny same-topic factual correction, retrieval still surfaces the
// chunk, and the CORRECTED content is what gets synthesized/cited. A full
// re-embed folds into the deferred incremental-update rebuild.
const CorpusDigestExpected = "eacdc94141566e22ab978a2c0728d834379f9559cffaca7a7a7f71508f83a2c8"

// EmbeddingDigestExpected pins the hybrid retrieval embedding sidecar produced by
// scripts/rag_w0/build_corpus_embeddings.py over the CorpusDigestExpected corpus
// with text-embedding-3-large (3072-dim). Mismatch indicates the sidecar is
// stale relative to the deployed corpus and RAG hybrid path must refuse to load.
const EmbeddingDigestExpected = "9dcb902bb6026836b43cf52be159af6690bb4c93818e1b34915f053818b9189c"

// EmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar produced by
// the same script over the CorpusDigestExpected corpus (--embed-model
// qwen3-embedding-8b, 4096-dim default). Selected only when
// RAG_RETRIEVAL_MODE=qwen3_full; the text-emb-3 sidecar above remains the
// default for hybrid_cosine / hybrid_rerank modes. Same mismatch semantics
// as EmbeddingDigestExpected: stale sidecar = hybrid path refuses to load.
const EmbeddingDigestExpectedQwen3 = "da488ead7fb53b6d7ab2e7529b9724b1a6f60910aeef253028db624c7dcd99b4"

// ExternalCorpusDigestExpected pins deploy/kb/external_w0.jsonl — the separate
// external tool/ops corpus (vLLM / SGLang / Ollama / ComfyUI + GPU
// troubleshooting + Linux-ops/env-management + PyTorch-basics + model-download
// + chat-seeded ops gaps: SSH keepalive, large transfers, remote web apps,
// HuggingFace cache/downloads, LoRA/QLoRA, NCCL/DDP debugging, and
// AI4Science/professional GPU runtime topics: JAX, CuPy, OpenMM, GROMACS,
// RAPIDS, container GPU access, ColabFold, and AlphaFold3 + pro-GPU support
// topics: Transformers/Accelerate/bitsandbytes/LLaMA-Factory/Unsloth/DeepSpeed,
// git-lfs/aria2/wget/curl/rclone, VS Code Remote/Jupyter/SSH tunnels,
// CUDA/NCCL compatibility, ComfyUI Manager/Flux/ControlNet/IPAdapter, and
// production GPU support: DCGM/Nsight monitoring, Docker/Kubernetes/GPU Operator,
// MIG/time-slicing, Triton/TensorRT-LLM/KServe, Ray/Slurm, TRL, HF Datasets,
// Zarr, Dask-cuDF, and community-image-targeted coverage for popular LTX,
// digital-human, voice/TTS, LoRA-training, Qwen/Wan, GGUF, and 3D generation
// images, plus second-wave coverage for SD/LoRA training, ASR/video dubbing,
// 3D/CV, AI4Science specialties, experiment tracking, proxying, and image
// building, plus 637-session-targeted support gaps: SD-WebUI/A1111, ControlNet,
// generic WebUI refused-connection triage, SSH/transfer failures, Ollama cache
// and context/VRAM issues, Open WebUI/LiteLLM, Docker GPU visibility, card-count
// mismatch, MPS, DVC/object storage/labeling, persistence boundaries, and
// background service patterns, plus focused coverage for safe nvidia-smi cleanup,
// torch.compile, and CUDA Toolkit installation, plus third-wave production and
// research platform coverage: FSDP/checkpointing/memory estimation/offload,
// vLLM/LiteLLM serving governance, NCCL/RDMA validation, Kueue/Kubeflow/Volcano
// job queues, lm-eval/MLflow evaluation, YOLO/SAM2 CV workflows, LAMMPS/PySCF/
// Apptainer AI4Science, WebDataset, ONNX Runtime/Optimum, AWQ/GPTQ/GGUF model
// format guidance), loaded
// alongside the platform corpus via LoadPinnedCorporaWithEmbeddings. Same
// refuse-to-start-on-mismatch semantics as the platform pin.
const ExternalCorpusDigestExpected = "d76f2cc633987cac4c88bcb3339ea50e262099a7eb14995e7a90b030ab909d38"

// ExternalEmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar
// (4096-dim) for the external corpus:
// deploy/kb/embeddings_<ExternalCorpusDigestExpected>_qwen3-embedding-8b.jsonl.
const ExternalEmbeddingDigestExpectedQwen3 = "d219162444ae434f213183add8c47adc9b804365d7818cb1bf6fd4d1fcc1b076"

// ComputeCorpusDigest normalizes line endings so the pinned corpus digest is
// stable across Windows and Unix checkouts.
func ComputeCorpusDigest(reader io.Reader) (string, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("compute corpus digest: %w", err)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	hash := sha256.New()
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ComputeCorpusFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open corpus for digest: %w", err)
	}
	defer file.Close()
	return ComputeCorpusDigest(file)
}

// ComputeEmbeddingFileDigest mirrors ComputeCorpusFileDigest semantics so the
// embedding sidecar pin is byte-stable across CRLF/LF checkouts.
func ComputeEmbeddingFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open embedding sidecar for digest: %w", err)
	}
	defer file.Close()
	return ComputeCorpusDigest(file)
}

func LoadPinnedCorpus(path string) (Corpus, error) {
	digest, err := ComputeCorpusFileDigest(path)
	if err != nil {
		return Corpus{}, err
	}
	if digest != CorpusDigestExpected {
		return Corpus{}, fmt.Errorf("corpus digest mismatch: got %s want %s", digest, CorpusDigestExpected)
	}
	return LoadCorpus(path)
}
