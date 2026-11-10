# 7. Roadmap MVP

## MVP-0 — Documentación controlada

- Estructura de repositorio y glosario.
- Constitución, contratos y estados.
- Arquitectura y límites de seguridad.
- ADRs iniciales.
- Matriz de hipótesis y plan de validación.

## MVP-1 — Núcleo local simulado

- API mínima y persistencia local. Motor de almacenamiento pendiente de decisión en la [ADR-0005](adr/0005-local-baseline.md).
- Motor de estados y eventos.
- Adaptador simulado.
- Worktree manager con operaciones seguras.
- Verificador determinista.
- Dashboard básico de una ejecución.

## MVP-2 — Adaptadores supervisados

- Adaptador Codex CLI.
- Adaptador Claude Code.
- Sesiones autenticadas existentes, sin gestión de credenciales.
- Configuración por fase de modelos y fallback.
- Pausa y aprobación manual.

## MVP-3 — Verificación y recuperación

- Verificación independiente configurable.
- Checkpoints, reintentos y recuperación.
- Evidencia navegable y auditoría.
- Simulación de capacidades `known/estimated/unknown`.

## MVP-4 — Operación endurecida

- Políticas de seguridad revisadas.
- Métricas, límites y pruebas de fallo.
- Instalación reproducible.
- Evaluación de multiusuario y despliegue remoto solo si se justifica.
