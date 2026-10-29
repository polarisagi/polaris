// ══════════════════════════════════════════════════════════════════════════
// wav_recorder.js: 纯前端 16kHz 单声道 16-bit PCM WAV 录音机
// 优先使用 AudioWorklet 避免主线程阻塞，降级至 ScriptProcessorNode
// ══════════════════════════════════════════════════════════════════════════

// 编码 Float32Array PCM 为标准 16-bit 单声道 WAV Blob
export function encodeWAV(samples, sampleRate = 16000) {
  const numSamples = samples.length;
  const dataSize = numSamples * 2;
  const buffer = new ArrayBuffer(44 + dataSize);
  const view = new DataView(buffer);

  // 1. RIFF 标识
  writeString(view, 0, 'RIFF');
  view.setUint32(4, 36 + dataSize, true);
  writeString(view, 8, 'WAVE');

  // 2. fmt chunk
  writeString(view, 12, 'fmt ');
  view.setUint32(16, 16, true);             // Subchunk1Size (16 for PCM)
  view.setUint16(20, 1, true);              // AudioFormat (1 = PCM)
  view.setUint16(22, 1, true);              // NumChannels (1 = Mono)
  view.setUint32(24, sampleRate, true);     // SampleRate
  view.setUint32(28, sampleRate * 2, true); // ByteRate (SampleRate * NumChannels * BitsPerSample/8)
  view.setUint16(32, 2, true);              // BlockAlign (NumChannels * BitsPerSample/8)
  view.setUint16(34, 16, true);             // BitsPerSample (16-bit)

  // 3. data chunk
  writeString(view, 36, 'data');
  view.setUint32(40, dataSize, true);

  // 4. PCM 采样数据
  let offset = 44;
  for (let i = 0; i < numSamples; i++) {
    const s = Math.max(-1, Math.min(1, samples[i]));
    const val = s < 0 ? s * 0x8000 : s * 0x7FFF;
    view.setInt16(offset, val, true);
    offset += 2;
  }

  return new Blob([view], { type: 'audio/wav' });
}

function writeString(view, offset, string) {
  for (let i = 0; i < string.length; i++) {
    view.setUint8(offset + i, string.charCodeAt(i));
  }
}

// 降采样至目标采样率（默认 16kHz）
export function downsample(buffer, fromRate, toRate = 16000) {
  if (fromRate === toRate) {
    return buffer;
  }
  const ratio = fromRate / toRate;
  const newLength = Math.round(buffer.length / ratio);
  const result = new Float32Array(newLength);
  let offsetResult = 0;
  let offsetBuffer = 0;

  while (offsetResult < result.length) {
    const nextOffsetBuffer = Math.round((offsetResult + 1) * ratio);
    let accum = 0;
    let count = 0;
    for (let i = offsetBuffer; i < nextOffsetBuffer && i < buffer.length; i++) {
      accum += buffer[i];
      count++;
    }
    result[offsetResult] = count > 0 ? accum / count : 0;
    offsetResult++;
    offsetBuffer = nextOffsetBuffer;
  }
  return result;
}

// Inline AudioWorklet 代码，避免跨域或静态资源路径配置问题
const workletCode = `
class RecorderWorklet extends AudioWorkletProcessor {
  constructor() {
    super();
    this.port.onmessage = (e) => {};
  }
  process(inputs) {
    const input = inputs[0];
    if (input && input[0]) {
      this.port.postMessage(input[0]);
    }
    return true;
  }
}
registerProcessor('recorder-worklet', RecorderWorklet);
`;

export class WavRecorder {
  constructor(stream, targetSampleRate = 16000) {
    this.stream = stream;
    this.targetSampleRate = targetSampleRate;
    this.audioContext = null;
    this.source = null;
    this.workletNode = null;
    this.scriptNode = null;
    this.analyser = null;

    this.allBuffers = [];
    this.chunkBuffers = [];
  }

  async init() {
    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    this.audioContext = new AudioContextCtor();
    this.source = this.audioContext.createMediaStreamSource(this.stream);

    this.analyser = this.audioContext.createAnalyser();
    this.analyser.fftSize = 512;
    this.source.connect(this.analyser);

    // 尝试初始化 AudioWorklet
    let workletInitialized = false;
    if (this.audioContext.audioWorklet) {
      try {
        const blob = new Blob([workletCode], { type: 'application/javascript' });
        const url = URL.createObjectURL(blob);
        await this.audioContext.audioWorklet.addModule(url);
        URL.revokeObjectURL(url);

        this.workletNode = new AudioWorkletNode(this.audioContext, 'recorder-worklet');
        this.workletNode.port.onmessage = (e) => {
          this._handlePCMData(e.data);
        };
        this.source.connect(this.workletNode);
        this.workletNode.connect(this.audioContext.destination);
        workletInitialized = true;
      } catch (e) {
        console.warn('AudioWorklet initialization failed, fallback to ScriptProcessor:', e);
      }
    }

    if (!workletInitialized) {
      // 降级使用 ScriptProcessorNode
      const bufferSize = 4096;
      this.scriptNode = this.audioContext.createScriptProcessor(bufferSize, 1, 1);
      this.scriptNode.onaudioprocess = (e) => {
        const inputData = e.inputBuffer.getChannelData(0);
        this._handlePCMData(new Float32Array(inputData));
      };
      this.source.connect(this.scriptNode);
      this.scriptNode.connect(this.audioContext.destination);
    }
  }

  _handlePCMData(float32Array) {
    // 拷贝并保存
    const copy = new Float32Array(float32Array);
    this.allBuffers.push(copy);
    this.chunkBuffers.push(copy);
  }

  getVolume() {
    if (!this.analyser) return 0;
    const bufferLength = this.analyser.frequencyBinCount;
    const dataArray = new Uint8Array(bufferLength);
    this.analyser.getByteFrequencyData(dataArray);
    let sum = 0;
    for (let i = 0; i < bufferLength; i++) {
      sum += dataArray[i];
    }
    return sum / bufferLength;
  }

  flushChunk() {
    if (this.chunkBuffers.length === 0) return null;
    const blob = this._exportBlob(this.chunkBuffers);
    this.chunkBuffers = [];
    return blob;
  }

  flushAll() {
    if (this.allBuffers.length === 0) return null;
    const blob = this._exportBlob(this.allBuffers);
    this.allBuffers = [];
    this.chunkBuffers = [];
    return blob;
  }

  _exportBlob(buffers) {
    let totalLength = 0;
    for (const b of buffers) totalLength += b.length;
    const merged = new Float32Array(totalLength);
    let offset = 0;
    for (const b of buffers) {
      merged.set(b, offset);
      offset += b.length;
    }

    // 降采样至 16000Hz
    const downsampled = downsample(merged, this.audioContext.sampleRate, this.targetSampleRate);
    return encodeWAV(downsampled, this.targetSampleRate);
  }

  close() {
    if (this.workletNode) {
      this.workletNode.disconnect();
      this.workletNode = null;
    }
    if (this.scriptNode) {
      this.scriptNode.disconnect();
      this.scriptNode = null;
    }
    if (this.source) {
      this.source.disconnect();
      this.source = null;
    }
    if (this.stream) {
      this.stream.getTracks().forEach((track) => track.stop());
      this.stream = null;
    }
    if (this.audioContext && this.audioContext.state !== 'closed') {
      this.audioContext.close();
      this.audioContext = null;
    }
  }
}
