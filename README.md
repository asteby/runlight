<p align="center">
  <img src="brand/banner.png" alt="runlight" width="100%">
</p>

<p align="center"><b>Autoscaler ligero e inteligente para runners self-hosted de GitHub Actions.</b><br>
Pensado para VPS pequeños y compartidos: el CI ya no tumba producción.</p>

---

## El problema

Un VPS con producción **y** runners de CI: cada `go build`, cada `docker build` compite por CPU, memoria y disco con
tus servicios reales. Cinco runners persistentes sin coordinación = carga 10–12, swap al 100 %, correo en 502.

## La idea

`runlight` es **un solo orquestador** (un binario Go de ~8 MB, ~15 MB de RAM) que registra un *scale set* en GitHub y
lanza **runners efímeros** (uno por job) **solo cuando hay trabajo y el servidor tiene margen**. Sin webhooks, sin polling
REST: usa la API oficial de *scale sets* (`actions/scaleset`, long-poll).

- **Gobernador AIMD**: sube +1 runner cada 15 s si hay demanda y producción está tranquila; recorta a la mitad ante presión.
- **Mide la presión de producción, no la del host**: lee PSI (`some avg10`) del cgroup de producción
  (`--psi-cgroup /sys/fs/cgroup/system.slice`). El propio CI esperando CPU no lo congela.
- **Guardas duras**: reserva de memoria intocable, disco libre mínimo y una estimación de memoria por runner **aprendida**
  del pico real de cada job.
- **Seguro con tus jobs**: solo destruye un runner si GitHub confirma que está libre. Reiniciar el daemon no mata jobs.
- **Conserje de cachés** (`janitor`): topes LRU por caché, borrado de carpetas de runners muertos y modo emergencia si el
  disco baja del 12 %. Memoria constante (histograma de mtime en dos pasadas, sin cargar listas de ficheros).
- **Dos backends**: `docker` (contenedores aislados) y `systemd` (nativo en el host, para jobs de despliegue; un cgroup por runner).

## Panel de control (PWA)

SPA en **React 19** + `lucide-react`, compilada con esbuild y **embebida en el binario** (no hace falta Node en el servidor).

- Resumen: por qué decide lo que decide, en español claro, con cada condición medida.
- Jobs, runners vivos, cola, lo aprendido, servidor, cachés y disco.
- **Alertas** (sin ruido: un solo aviso por problema, con «resuelto» al terminar) y **notificaciones push** (Web Push/VAPID).
- **Configuración editable en vivo** con validación por campo y registro de auditoría.
- Instalable como app; tema claro/oscuro; menú colapsable; móvil con barra de pestañas.
- Login propio: PBKDF2-SHA256 (600 000 iteraciones), cookie `HttpOnly`/`Secure`/`SameSite=Strict`, bloqueo por IP,
  protección CSRF (origen + cabecera + `Content-Type`), CSP estricta.

## Instalación rápida

Requisitos: Linux con cgroups v2 y PSI, Go ≥ 1.27 (solo para compilar) y un token con permisos sobre la org o el repo.

```bash
go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always)" -o runlight ./cmd/runlight
sudo install -m 0755 runlight /usr/local/bin/runlight

sudo install -d -m 0700 /etc/runlight /var/lib/runlight
sudo tee /etc/runlight/env >/dev/null <<'ENV'
RUNLIGHT_TOKEN=<token de GitHub>
RUNLIGHT_DASH_USER=admin
RUNLIGHT_DASH_PASSWORD_HASH=<salida de: runlight --hash-password>
RUNLIGHT_SESSION_KEY=<cadena aleatoria larga>
ENV
sudo chmod 0600 /etc/runlight/env
```

Después copia `deploy/runlight.service` y `deploy/runlight.slice` a `/etc/systemd/system/`, ajusta los flags y ejecuta
`systemctl enable --now runlight`. En tus workflows basta con `runs-on: runlight` (o las labels que fijes con `--labels`).

> El panel escucha en `127.0.0.1:9128`. Publícalo detrás de un proxy con HTTPS (nginx, Cloudflare…): las notificaciones
> push y la instalación como app requieren HTTPS.

### Flags principales

| Flag | Para qué |
|---|---|
| `--url` | Org/repo/enterprise que sirve (`RUNLIGHT_URL`). |
| `--name` | Nombre del scale set; el que pones en `runs-on:`. |
| `--min` / `--max` | Runners calientes / techo (0 = automático por CPU/RAM). |
| `--backend` | `docker` o `systemd`. |
| `--mem-per-runner-mb`, `--mem-reserve-mb`, `--disk-min-free-mb` | Presupuesto de memoria y guardas. |
| `--psi-cgroup`, `--psi-cpu-high`, `--psi-mem-high`, `--psi-io-high` | Qué presión medir y desde cuándo frenar. |
| `--cache`, `--cache-ephemeral`, `--janitor-interval`, `--janitor-dry-run` | Conserje de cachés (prueba siempre antes con `--janitor-dry-run`). |
| `--listen`, `--dash-user`, `--hash-password` | Panel y login. |
| `--state-dir`, `--vapid-subject` | Estado persistente (push, ajustes) y contacto VAPID. |

`runlight --help` los lista todos. Muchos umbrales también se editan en caliente desde el panel (Configuración).

## Desarrollo

```bash
go vet ./... && go test ./...                                # backend
cd internal/dashboard/web-src && npm ci && npm run build     # regenera internal/dashboard/web/app.js
go run ./cmd/dashdemo --listen 127.0.0.1:9128 --user admin --password demo-pass-123   # panel con datos sintéticos
```

- `internal/{governor,scaler,backend,janitor,sysmon,telemetry,alerts,push,settings,dashboard}`: cada pieza es un paquete pequeño con tests.
- `internal/dashboard/web-src/`: fuentes React (JSX). `internal/dashboard/web/`: salida versionada y estáticos, embebidos con `go:embed`.
- `brand/`: logo (cuadrícula 4×4 que dibuja una **R**), exportaciones y `sync.sh`. Tipografía [Space Grotesk](https://github.com/floriankarsten/space-grotesk) (SIL OFL, incluida en `web/fonts/`).

## Seguridad

- El token de GitHub vive en un archivo `0600`, nunca en argv ni en la unidad systemd. El JIT de cada runner va por archivo `0600`.
- Las suscripciones push se validan contra una lista blanca de servicios (anti-SSRF) y nunca se devuelven claves ni endpoints completos.
- Reporta vulnerabilidades de forma privada (Security → *Report a vulnerability* en GitHub).

## Licencia

[MIT](LICENSE). Space Grotesk: SIL Open Font License 1.1 (`internal/dashboard/web/fonts/OFL.txt`).
