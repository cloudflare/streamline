export type StreamOperation =
  | {
      op: 'overlay'
      params:
        | { image: '/app/assets/streamline-logo.png'; position: 'top-right' }
        | { image: 'annotation'; position: 'full' }
    }
  | {
      op: 'subtitle'
      params: {
        source: 'auto'
      }
    }
  | {
      op: 'filter'
      params:
        | { preset: 'blur' | 'brightness' | 'contrast' | 'gamma' | 'saturation' | 'sharpen'; amount: number }
        | { preset: 'flip' }
        | { preset: 'rotate'; degrees: 0 | 90 | 180 | 270 }
    }
  | {
      op: 'encode'
      params: {
        codec: 'h264'
        preset?: 'ultrafast' | 'superfast' | 'veryfast' | 'faster' | 'fast' | 'medium'
        bitrate?: string
        resolution?: string
        fps?: number
        gop?: number
      }
    }

export type StreamlineInput =
  | { type: 'webcam' }
  | { type: 'hls'; url: string }
  | { type: 'rtmp'; profile: string }

export type StreamlineDirectInput = Exclude<StreamlineInput, { type: 'webcam' }>

export interface StreamlineInputTransform {
  scale: number
  position: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right'
}

export type StreamlineCompositeInputs = [
  StreamlineDirectInput,
  { type: 'webcam'; transform: StreamlineInputTransform },
]

export type StreamlineOutput =
  | { mode: 'websocket'; format?: 'fmp4' }
  | { mode: 'rtmp'; profile: string }

interface StreamlineStartBase {
  pipeline: StreamOperation[]
  output: StreamlineOutput
}

export type StreamlineStartConfig = StreamlineStartBase & (
  | { input: StreamlineInput; inputs?: never }
  | { input?: never; inputs: StreamlineCompositeInputs }
)
