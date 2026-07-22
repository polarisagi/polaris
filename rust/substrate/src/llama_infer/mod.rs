// llama_infer/mod.rs — 本地推理模块（P3-1，tier1 feature 门控）
// 架构文档: docs/arch/M01-Inference-Runtime.md §8
//
// 设计原则:
//   - 单模型槽位（Mutex<Option<ModelHolder>>）：与 Go 侧 "/model local <id>"
//     热切换语义对齐——load 前若已有模型，先完整卸载旧模型资源，不支持
//     同时常驻多模型（Tier-1 硬件显存/内存有限，多模型常驻不具备性价比）。
//   - 持久化生成上下文（gen_ctx）：上下文创建（KV cache 缓冲区分配）是重量级
//     操作，跨请求复用同一 LlamaContext 避免每次 generate 重复分配；但不做
//     跨请求增量前缀复用（无 KV 前缀匹配/位置重排逻辑），因此每次 generate
//     开头显式 clear_kv_cache 保证正确性。EvictKVCache 作为独立 FFI 暴露，
//     供 Go 侧在会话切换/内存回收场景主动触发。
//   - 生命周期自持有：llama-cpp-2 的 LlamaContext<'a> 生命周期绑定到 &'a LlamaModel，
//     无法在 Mutex<Option<T>> 全局态中直接安全共存（自引用结构体问题）。
//     此处用 Box::leak 获得 'static 模型引用，手动控制析构顺序
//     （见 unload() 注释），是本项目在无 unsafe 自引用 crate 依赖前提下
//     的标准解法，与 ffi 目录下其它模块（native_sandbox/surreal_store）
//     一致地把 unsafe 集中在薄薄一层、并用注释标注不变量。

use std::num::NonZeroU32;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

use llama_cpp_2::context::LlamaContext;
use llama_cpp_2::context::params::{LlamaContextParams, LlamaPoolingType};
use llama_cpp_2::llama_backend::LlamaBackend;
use llama_cpp_2::llama_batch::LlamaBatch;
use llama_cpp_2::model::params::LlamaModelParams;
use llama_cpp_2::model::{AddBos, LlamaChatMessage, LlamaChatTemplate, LlamaModel};
use llama_cpp_2::sampling::LlamaSampler;
use serde::{Deserialize, Serialize};

pub mod dispatch;

// ─── 全局后端 ──────────────────────────────────────────────────────────────
// llama_backend_init 只应在进程生命周期内调用一次。用 OnceLock<Result<...>>
// （而非先探测 get() 再条件调用 init()）是关键：并发首次调用下若把 init()
// 放在 get_or_init 闭包之外，会出现竞态——两个线程都看到 get()==None 从而
// 都调用 LlamaBackend::init()，其中失败的一方会收到 llama.cpp 返回的
// "BackendAlreadyInitialized" 错误并被错误地当作真实失败传播出去。
// get_or_init 的闭包由 OnceLock 保证全进程只执行一次，从根本上消除该竞态
// （此 bug 在本模块单元测试并发执行时被实际触发过一次，见测试注释）。
static BACKEND: OnceLock<Result<LlamaBackend, String>> = OnceLock::new();

fn backend() -> Result<&'static LlamaBackend, String> {
    let cell = BACKEND.get_or_init(|| {
        LlamaBackend::init().map_err(|e| format!("LlamaBackend::init failed: {e}"))
    });
    cell.as_ref().map_err(|e| e.clone())
}

// ─── 模型持有者 ────────────────────────────────────────────────────────────

struct ModelHolder {
    // 字段本身声明顺序不承载析构语义（我们不用自动 Drop，见 unload()），
    // 但保留 gen_ctx 在前，提醒读者其生命周期先于 model_ptr 结束。
    gen_ctx: LlamaContext<'static>,
    model_ptr: *mut LlamaModel,
    path: String,
    n_ctx: u32,
    n_gpu_layers: u32,
    n_ctx_train: u32,
    n_embd: i32,
}

// Safety: ModelHolder 的所有访问都经由 STATE: Mutex<Option<ModelHolder>> 序列化，
// 不存在跨线程并发访问裸指针/LlamaContext 内部状态的场景；llama.cpp 底层 C API
// 允许"不同线程依次调用"（非并发调用同一 context 即可），故标记 Send 是可靠的。
unsafe impl Send for ModelHolder {}

static STATE: Mutex<Option<ModelHolder>> = Mutex::new(None);
static ABORT_FLAG: AtomicBool = AtomicBool::new(false);

fn lock_state() -> Result<MutexGuard<'static, Option<ModelHolder>>, String> {
    STATE.lock().map_err(|_| {
        "llama_infer: model state mutex poisoned (前次调用 panic 导致状态可能不一致，需重启进程)"
            .to_string()
    })
}

/// unload 显式控制析构顺序：先 drop gen_ctx（释放 llama_context 持有的 KV cache/
/// 计算缓冲区），再回收 Box::leak 出去的模型内存。二者若顺序颠倒，gen_ctx 内部
/// 对模型的引用将悬空（虽然 llama_free(ctx) 本身可能不触碰模型内存，但不应
/// 依赖这一未在文档中承诺的实现细节）。
fn unload(holder: ModelHolder) {
    let ModelHolder {
        gen_ctx, model_ptr, ..
    } = holder;
    drop(gen_ctx);
    unsafe {
        drop(Box::from_raw(model_ptr));
    }
}

fn rand_seed() -> u32 {
    static COUNTER: AtomicU32 = AtomicU32::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    nanos
        ^ COUNTER
            .fetch_add(1, Ordering::Relaxed)
            .wrapping_mul(2_654_435_761)
}

// ─── load_model ───────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct LoadModelRequest {
    pub model_path: String,
    #[serde(default)]
    pub n_ctx: u32,
    #[serde(default)]
    pub n_gpu_layers: u32,
    #[serde(default)]
    pub n_threads: i32,
}

#[derive(Serialize)]
pub struct LoadModelResponse {
    pub ok: bool,
    pub n_ctx: u32,
    pub n_ctx_train: u32,
    pub n_embd: i32,
    pub n_params: u64,
}

pub fn load_model(req: LoadModelRequest) -> Result<LoadModelResponse, String> {
    let be = backend()?;

    ABORT_FLAG.store(false, Ordering::Relaxed);

    // 若已有模型常驻，先完整卸载（热切换语义：单槽位）。
    {
        let mut guard = lock_state()?;
        if let Some(old) = guard.take() {
            unload(old);
        }
    }

    let model_params = LlamaModelParams::default().with_n_gpu_layers(req.n_gpu_layers);
    let model = LlamaModel::load_from_file(be, &req.model_path, &model_params)
        .map_err(|e| format!("load_from_file failed: {e}"))?;
    let n_ctx_train = model.n_ctx_train();
    let n_embd = model.n_embd();
    let n_params = model.n_params();

    let model_ptr: *mut LlamaModel = Box::into_raw(Box::new(model));
    // Safety: model_ptr 指向的分配由本模块独占管理；下方构造的 'static 引用
    // 仅在 STATE 持有对应 ModelHolder 期间使用，unload() 保证先释放所有
    // 借用该引用的 LlamaContext，再释放模型本身。
    let model_static: &'static LlamaModel = unsafe { &*model_ptr };

    let n_ctx_req = if req.n_ctx > 0 {
        req.n_ctx
    } else {
        n_ctx_train
    };
    let ctx_n = NonZeroU32::new(n_ctx_req.max(1));
    let mut ctx_params = LlamaContextParams::default().with_n_ctx(ctx_n);
    if req.n_threads > 0 {
        ctx_params = ctx_params
            .with_n_threads(req.n_threads)
            .with_n_threads_batch(req.n_threads);
    }

    let gen_ctx = match model_static.new_context(be, ctx_params) {
        Ok(c) => c,
        Err(e) => {
            // 上下文创建失败必须手动回收已 leak 的模型内存，否则泄漏。
            unsafe {
                drop(Box::from_raw(model_ptr));
            }
            return Err(format!("new_context failed: {e}"));
        }
    };

    let holder = ModelHolder {
        gen_ctx,
        model_ptr,
        path: req.model_path.clone(),
        n_ctx: n_ctx_req,
        n_gpu_layers: req.n_gpu_layers,
        n_ctx_train,
        n_embd,
    };

    let mut guard = lock_state()?;
    *guard = Some(holder);

    Ok(LoadModelResponse {
        ok: true,
        n_ctx: n_ctx_req,
        n_ctx_train,
        n_embd,
        n_params,
    })
}

pub fn unload_model() -> Result<(), String> {
    ABORT_FLAG.store(true, Ordering::Relaxed);
    let mut guard = lock_state()?;
    if let Some(holder) = guard.take() {
        unload(holder);
    }
    Ok(())
}

pub fn evict_kv_cache() -> Result<(), String> {
    let mut guard = lock_state()?;
    match guard.as_mut() {
        Some(holder) => {
            holder.gen_ctx.clear_kv_cache();
            Ok(())
        }
        None => Err("no model loaded".to_string()),
    }
}

#[derive(Serialize)]
pub struct StatusResponse {
    pub loaded: bool,
    pub path: String,
    pub n_ctx: u32,
    pub n_ctx_train: u32,
    pub n_embd: i32,
    pub n_gpu_layers: u32,
}

pub fn status() -> Result<StatusResponse, String> {
    let guard = lock_state()?;
    Ok(match guard.as_ref() {
        Some(h) => StatusResponse {
            loaded: true,
            path: h.path.clone(),
            n_ctx: h.n_ctx,
            n_ctx_train: h.n_ctx_train,
            n_embd: h.n_embd,
            n_gpu_layers: h.n_gpu_layers,
        },
        None => StatusResponse {
            loaded: false,
            path: String::new(),
            n_ctx: 0,
            n_ctx_train: 0,
            n_embd: 0,
            n_gpu_layers: 0,
        },
    })
}

// ─── generate ─────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ChatMessageIn {
    pub role: String,
    pub content: String,
}

#[derive(Deserialize)]
pub struct GenerateRequest {
    pub messages: Vec<ChatMessageIn>,
    #[serde(default)]
    pub max_tokens: i32,
    #[serde(default)]
    pub temperature: f32,
    #[serde(default)]
    pub top_p: f32,
    #[serde(default)]
    pub seed: u32,
    #[serde(default)]
    pub grammar: Option<String>,
    #[serde(default)]
    pub grammar_root: Option<String>,
    #[serde(default)]
    pub stop: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct GenerateResponse {
    pub text: String,
    pub prompt_tokens: i32,
    pub tokens_generated: i32,
    pub finish_reason: String,
}

pub fn generate(req: GenerateRequest) -> Result<GenerateResponse, String> {
    // 仅确保后端已初始化（生成复用持久化 gen_ctx，无需再次持有 backend 引用）。
    backend()?;
    ABORT_FLAG.store(false, Ordering::Relaxed);
    let mut guard = lock_state()?;
    let holder = guard
        .as_mut()
        .ok_or_else(|| "no model loaded".to_string())?;
    // Safety: model_ptr 在 holder 存活期间有效（unload 时才回收），此处借用
    // 与 holder 借用不重叠使用即可（下方对 gen_ctx 的可变借用与此不可变借用
    // 分别作用于不同字段，Rust 允许字段级拆借）。
    let model: &LlamaModel = unsafe { &*holder.model_ptr };

    let tmpl: LlamaChatTemplate = match model.chat_template(None) {
        Ok(t) => t,
        Err(_) => LlamaChatTemplate::new("chatml")
            .map_err(|e| format!("fallback chatml template invalid: {e}"))?,
    };

    let mut chat_msgs = Vec::with_capacity(req.messages.len());
    for m in &req.messages {
        let cm = LlamaChatMessage::new(m.role.clone(), m.content.clone())
            .map_err(|e| format!("invalid chat message: {e}"))?;
        chat_msgs.push(cm);
    }
    let prompt = model
        .apply_chat_template(&tmpl, &chat_msgs, true)
        .map_err(|e| format!("apply_chat_template failed: {e}"))?;

    let tokens = model
        .str_to_token(&prompt, AddBos::Always)
        .map_err(|e| format!("tokenize failed: {e}"))?;

    if tokens.len() as u32 >= holder.n_ctx {
        return Err(format!(
            "prompt too long: {} tokens >= n_ctx {}",
            tokens.len(),
            holder.n_ctx
        ));
    }

    // 每次 generate 前清空 KV cache（正确性优先于跨请求前缀复用，见模块头注释）。
    holder.gen_ctx.clear_kv_cache();

    let max_tokens = if req.max_tokens > 0 {
        req.max_tokens
    } else {
        512
    };
    let temperature = if req.temperature > 0.0 {
        req.temperature
    } else {
        0.8
    };
    let top_p = if req.top_p > 0.0 { req.top_p } else { 0.95 };
    let seed = if req.seed != 0 { req.seed } else { rand_seed() };

    let mut sampler_chain: Vec<LlamaSampler> = Vec::new();
    if let Some(g) = req.grammar.as_deref().filter(|s| !s.is_empty()) {
        let root = req.grammar_root.as_deref().unwrap_or("root");
        let gs =
            LlamaSampler::grammar(model, g, root).map_err(|e| format!("invalid grammar: {e}"))?;
        sampler_chain.push(gs);
    }
    sampler_chain.push(LlamaSampler::temp(temperature));
    sampler_chain.push(LlamaSampler::top_p(top_p, 1));
    sampler_chain.push(LlamaSampler::dist(seed));
    let mut sampler = LlamaSampler::chain(sampler_chain, false);

    let mut batch = LlamaBatch::new(holder.n_ctx as usize, 1);
    batch
        .add_sequence(&tokens, 0, true)
        .map_err(|e| format!("batch add_sequence failed: {e}"))?;
    holder
        .gen_ctx
        .decode(&mut batch)
        .map_err(|e| format!("initial decode failed: {e}"))?;

    let mut decoder = encoding_rs::UTF_8.new_decoder();
    let mut output = String::new();
    let mut pos: i32 = tokens.len() as i32;
    let mut generated: i32 = 0;
    let mut finish_reason = "length".to_string();

    for _ in 0..max_tokens {
        if ABORT_FLAG.load(Ordering::Relaxed) {
            finish_reason = "abort".to_string();
            break;
        }

        let last_idx = batch.n_tokens() - 1;
        let token = sampler.sample(&holder.gen_ctx, last_idx);
        sampler.accept(token);

        if model.is_eog_token(token) {
            finish_reason = "stop".to_string();
            break;
        }

        let piece = model
            .token_to_piece(token, &mut decoder, false, None)
            .map_err(|e| format!("token_to_piece failed: {e}"))?;
        output.push_str(&piece);
        generated += 1;

        if !req.stop.is_empty() {
            if let Some(hit) = req
                .stop
                .iter()
                .find(|s| !s.is_empty() && output.ends_with(s.as_str()))
            {
                let cut = output.len() - hit.len();
                output.truncate(cut);
                finish_reason = "stop".to_string();
                break;
            }
        }

        batch.clear();
        batch
            .add(token, pos, &[0], true)
            .map_err(|e| format!("batch add failed: {e}"))?;
        holder
            .gen_ctx
            .decode(&mut batch)
            .map_err(|e| format!("decode failed: {e}"))?;
        pos += 1;
    }

    Ok(GenerateResponse {
        text: output,
        prompt_tokens: tokens.len() as i32,
        tokens_generated: generated,
        finish_reason,
    })
}

// ─── embed ────────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct EmbedRequest {
    pub texts: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct EmbedResponse {
    pub embeddings: Vec<Vec<f32>>,
    pub n_embd: i32,
}

pub fn embed(req: EmbedRequest) -> Result<EmbedResponse, String> {
    let be = backend()?;
    let guard = lock_state()?;
    let holder = guard
        .as_ref()
        .ok_or_else(|| "no model loaded".to_string())?;
    let model: &LlamaModel = unsafe { &*holder.model_ptr };
    let n_embd = model.n_embd();

    // 嵌入使用独立的临时 LlamaContext（embeddings=true + Mean pooling），
    // 与常驻的 gen_ctx（生成用途）互不干扰，调用结束后随作用域释放。
    let ctx_n = NonZeroU32::new(holder.n_ctx.max(1));
    let ctx_params = LlamaContextParams::default()
        .with_n_ctx(ctx_n)
        .with_embeddings(true)
        .with_pooling_type(LlamaPoolingType::Mean);
    let mut embed_ctx = model
        .new_context(be, ctx_params)
        .map_err(|e| format!("embedding new_context failed: {e}"))?;

    let mut out = Vec::with_capacity(req.texts.len());
    for text in &req.texts {
        let tokens = model
            .str_to_token(text, AddBos::Always)
            .map_err(|e| format!("tokenize failed: {e}"))?;
        if tokens.is_empty() {
            out.push(vec![0.0f32; n_embd.max(0) as usize]);
            continue;
        }
        embed_ctx.clear_kv_cache();
        let mut batch = LlamaBatch::new(tokens.len(), 1);
        batch
            .add_sequence(&tokens, 0, true)
            .map_err(|e| format!("batch add_sequence failed: {e}"))?;
        embed_ctx
            .decode(&mut batch)
            .map_err(|e| format!("embedding decode failed: {e}"))?;
        let vec = embed_ctx
            .embeddings_seq_ith(0)
            .map_err(|e| format!("embeddings_seq_ith failed: {e}"))?
            .to_vec();
        out.push(vec);
    }
    Ok(EmbedResponse {
        embeddings: out,
        n_embd,
    })
}

// ─── rerank ───────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct RerankRequest {
    pub query: String,
    pub documents: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct RerankResponse {
    pub scores: Vec<f32>,
}

/// rerank 通过复用 embed() 的批量嵌入 + 余弦相似度实现通用双塔式重排序。
/// llama.cpp/GGUF 生态缺少统一的 cross-encoder 分类头调用约定，双塔嵌入
/// 相似度是能用任意已加载 embedding 模型实现、不引入额外模型格式假设的
/// 最稳健通用方案（见 docs/arch/M01-Inference-Runtime.md §8）。
pub fn rerank(req: RerankRequest) -> Result<RerankResponse, String> {
    let mut all_texts = Vec::with_capacity(1 + req.documents.len());
    all_texts.push(req.query.clone());
    all_texts.extend(req.documents.iter().cloned());
    let embedded = embed(EmbedRequest { texts: all_texts })?;
    if embedded.embeddings.is_empty() {
        return Ok(RerankResponse { scores: Vec::new() });
    }
    let query_vec = &embedded.embeddings[0];
    let scores = embedded.embeddings[1..]
        .iter()
        .map(|doc_vec| cosine(query_vec, doc_vec))
        .collect();
    Ok(RerankResponse { scores })
}

fn cosine(a: &[f32], b: &[f32]) -> f32 {
    if a.is_empty() || a.len() != b.len() {
        return 0.0;
    }
    let mut dot = 0.0f32;
    let mut na = 0.0f32;
    let mut nb = 0.0f32;
    for i in 0..a.len() {
        dot += a[i] * b[i];
        na += a[i] * a[i];
        nb += b[i] * b[i];
    }
    let denom = na.sqrt() * nb.sqrt();
    if denom == 0.0 { 0.0 } else { dot / denom }
}

// ─── 单元测试 ──────────────────────────────────────────────────────────────
// 注：真实权重端到端生成/嵌入测试需要 GGUF 模型文件（体积 MB~GB 级），
// 不适合作为默认 `cargo test` 的一部分（违背 Tier-0 最小化 CI footprint 原则）。
// 此处仅覆盖"未加载模型"错误路径与纯函数（cosine），与 Go 侧
// TestLocalAdapter_* 的门控/降级测试互补；真实推理正确性验证走手动/CI
// opt-in 集成测试（下载模型 fixture），不在本模块范围内。

#[cfg(test)]
mod tests {
    use super::*;

    // llama_infer 全局 STATE 在整个测试二进制内共享；本模块所有测试都刻意
    // 避免调用 load_model（无 GGUF fixture），因此无需额外加锁串行化。

    #[test]
    fn test_status_when_not_loaded() {
        let s = status().expect("status should not error");
        assert!(!s.loaded);
        assert_eq!(s.path, "");
        assert_eq!(s.n_ctx, 0);
    }

    #[test]
    fn test_generate_without_model_returns_error() {
        let req = GenerateRequest {
            messages: vec![ChatMessageIn {
                role: "user".to_string(),
                content: "hi".to_string(),
            }],
            max_tokens: 8,
            temperature: 0.0,
            top_p: 0.0,
            seed: 0,
            grammar: None,
            grammar_root: None,
            stop: vec![],
        };
        let err = generate(req).expect_err("should error without loaded model");
        assert!(err.contains("no model loaded"), "unexpected error: {err}");
    }

    #[test]
    fn test_embed_without_model_returns_error() {
        let req = EmbedRequest {
            texts: vec!["hello".to_string()],
        };
        let err = embed(req).expect_err("should error without loaded model");
        assert!(err.contains("no model loaded"), "unexpected error: {err}");
    }

    #[test]
    fn test_rerank_without_model_returns_error() {
        let req = RerankRequest {
            query: "q".to_string(),
            documents: vec!["d1".to_string()],
        };
        let err = rerank(req).expect_err("should error without loaded model");
        assert!(err.contains("no model loaded"), "unexpected error: {err}");
    }

    #[test]
    fn test_evict_kv_cache_without_model_returns_error() {
        let err = evict_kv_cache().expect_err("should error without loaded model");
        assert!(err.contains("no model loaded"), "unexpected error: {err}");
    }

    #[test]
    fn test_unload_without_model_is_noop_ok() {
        // 未加载模型时 unload 应是幂等 no-op，不报错。
        assert!(unload_model().is_ok());
    }

    #[test]
    fn test_cosine_identical() {
        let a = [1.0_f32, 0.0, 0.0];
        let r = cosine(&a, &a);
        assert!((r - 1.0).abs() < 1e-6);
    }

    #[test]
    fn test_cosine_orthogonal() {
        let a = [1.0_f32, 0.0];
        let b = [0.0_f32, 1.0];
        assert!(cosine(&a, &b).abs() < 1e-6);
    }

    #[test]
    fn test_cosine_mismatched_len_returns_zero() {
        let a = [1.0_f32, 0.0];
        let b = [1.0_f32, 0.0, 0.0];
        assert_eq!(cosine(&a, &b), 0.0);
    }

    #[test]
    fn test_cosine_empty_returns_zero() {
        let a: [f32; 0] = [];
        assert_eq!(cosine(&a, &a), 0.0);
    }
}
