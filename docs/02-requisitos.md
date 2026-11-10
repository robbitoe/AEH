# 2. Requisitos

## Funcionales

### RF-01 — Constitución de producto

El sistema debe permitir expresar propósito, principios, no objetivos, criterios de éxito, restricciones y política de aprobación.

### RF-02 — Outcome

Cada outcome debe tener descripción, alcance, criterios de aceptación, evidencia esperada, riesgos y estado.

### RF-03 — Fases

Un outcome debe poder descomponerse en fases ordenadas o condicionadas. Cada fase declara ejecutor, modelo preferido, modelo de reserva, permisos, timeout, política de reintento y verificador.

### RF-04 — Adaptadores

Codex CLI y Claude Code deben modelarse como adaptadores externos sustituibles. AEH debe registrar la invocación, salida, exit code, duración y evidencia disponible.

### RF-05 — Capacidad

El sistema debe representar la disponibilidad de cuota/tokens sin falsear precisión. Si el proveedor no la expone, el valor debe ser `unknown` y la política debe decidir si continuar, pausar o cambiar de ejecutor.

### RF-06 — Aislamiento

Cada ejecución debe poder asociarse a un worktree o sandbox y registrar su base, rama, cambios y limpieza.

### RF-07 — Verificación

La verificación debe poder ejecutarse como etapa independiente y producir un dictamen `pass`, `fail` o `needs_review` con evidencia. El coordinador aplica la transición de ejecución según el dictamen y la política.

### RF-08 — Observabilidad

El sistema debe registrar estados, eventos, decisiones, artefactos y errores de forma consultable.

### RF-09 — Recuperación

Una ejecución pausada o fallida debe poder reanudarse desde un checkpoint o marcarse como no recuperable con causa.

## No funcionales iniciales

- **Trazabilidad:** cada decisión relevante debe apuntar a su causa y evidencia.
- **Seguridad:** mínimo privilegio, secretos fuera de logs, aprobación para acciones de riesgo.
- **Determinismo operativo:** contratos versionados e idempotencia donde sea posible.
- **Portabilidad:** adaptadores desacoplados de un proveedor específico.
- **Honestidad de estado:** nunca presentar estimaciones como datos confirmados.
