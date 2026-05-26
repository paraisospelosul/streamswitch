# StreamSwitch

**H.265 Stream Failover Relay com Web UI**

Recebe um stream SRT (H.265/HEVC), monitora o fluxo de dados, e troca automaticamente para um vídeo de fallback quando o stream cai. Envia para múltiplos destinos RTMP com passthrough H.265 ou transcoding H.264.

## Arquitetura

```
Moblin (H.265) → SRT:8282 → SRTLA/Belabox → SRT → [StreamSwitch] → RTMP
                                                         ↑
                                                    [Fallback .ts]
```

### Como funciona o failover

1. **StreamSwitch** recebe o fluxo SRT via FFmpeg e lê pacotes MPEGTS
2. Monitora continuamente se dados estão chegando
3. Se o SRT parar por mais de 2 segundos (configurável):
   - Troca para o fallback no **próximo keyframe** (zero artefatos)
   - O fallback é um arquivo `.ts` pré-encodado em H.265 com os mesmos parâmetros
4. Quando o SRT volta:
   - Espera um keyframe no fluxo SRT
   - Troca de volta para o stream ao vivo

### Tipos de saída

| Tipo | CPU | Uso |
|------|-----|-----|
| **H.265 Passthrough** | ~0% | YouTube (Enhanced RTMP) |
| **H.264 Transcode** | ~50% (2 cores) | Twitch, Kick, Facebook |

## Instalação rápida (VPS Oracle)

```bash
git clone <repo> /opt/streamswitch
cd /opt/streamswitch
bash scripts/install.sh
```

O script instala: FFmpeg 7.0+, Go, compila o StreamSwitch, e configura o systemd.

## Uso manual

### 1. Criar o arquivo de fallback

A partir de uma imagem:
```bash
bash scripts/create_fallback.sh brb.png
```

A partir de um vídeo:
```bash
bash scripts/create_fallback.sh -d 120 meu_video.mp4
```

### 2. Compilar

```bash
go mod tidy
go build -o streamswitch .
```

### 3. Executar

```bash
# Conectar ao SRTLA na porta 8282 (modo caller)
sudo ./streamswitch --fallback fallback.ts

# Escutar SRT na porta 5000 (modo listener)
sudo ./streamswitch --srt-mode listener --srt-addr 0.0.0.0:5000 --fallback fallback.ts

# Porta web customizada
sudo ./streamswitch --fallback fallback.ts --port 8080
```

### 4. Acessar a Web UI

Abra `http://SEU_IP` no navegador.

## Flags

| Flag | Default | Descrição |
|------|---------|-----------|
| `--srt-addr` | `localhost:8282` | Endereço SRT (host:porta) |
| `--srt-mode` | `caller` | Modo SRT: `caller` ou `listener` |
| `--fallback` | *(obrigatório)* | Caminho do arquivo MPEGTS de fallback |
| `--port` | `80` | Porta da Web UI |
| `--srt-timeout` | `2000` | Timeout SRT em ms antes de trocar para fallback |
| `--data-dir` | (dir do binário) | Diretório para configs e dados |

## API REST

| Método | Endpoint | Descrição |
|--------|----------|-----------|
| `GET` | `/api/status` | Status geral (switcher + outputs) |
| `GET` | `/api/outputs` | Lista saídas |
| `POST` | `/api/outputs` | Adiciona saída |
| `PUT` | `/api/outputs/{id}` | Atualiza saída |
| `DELETE` | `/api/outputs/{id}` | Remove saída |
| `POST` | `/api/outputs/{id}/start` | Inicia saída |
| `POST` | `/api/outputs/{id}/stop` | Para saída |

### Exemplo: Adicionar saída YouTube (H.265)

```bash
curl -X POST http://localhost/api/outputs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "YouTube 1",
    "url": "rtmp://a.rtmp.youtube.com/live2",
    "stream_key": "SUA_CHAVE",
    "codec": "h265"
  }'
```

### Exemplo: Adicionar saída Twitch (H.264)

```bash
curl -X POST http://localhost/api/outputs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Twitch",
    "url": "rtmp://live.twitch.tv/app",
    "stream_key": "SUA_CHAVE",
    "codec": "h264",
    "bitrate": 6000,
    "preset": "ultrafast"
  }'
```

## Requisitos do VPS

- **OS:** Ubuntu 22.04+ ou Oracle Linux 9
- **CPU:** 4 cores (ARM Ampere recomendado)
- **RAM:** 4GB+ (24GB ideal)
- **FFmpeg:** 7.0+ (para Enhanced RTMP H.265)
- **Rede:** Porta 80 TCP (Web UI), porta SRT (UDP)

## Requisitos do fallback

O arquivo `.ts` de fallback **deve** ter:
- Codec: H.265 (HEVC)
- Resolução: Mesma do stream (1920x1080)
- FPS: Mesmo do stream (30)
- Keyframe interval: 2 segundos (60 frames)
- Áudio: AAC, 48kHz, stereo
- Container: MPEGTS

Use o script `create_fallback.sh` para garantir compatibilidade.

## Licença

MIT
