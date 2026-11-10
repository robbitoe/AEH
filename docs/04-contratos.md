# 4. Contratos

Los ejemplos YAML son ilustrativos. Las reglas de versionado y evidencia están propuestas en la [ADR-0006](adr/0006-execution-record.md).

## Product Constitution

Campos mínimos propuestos: identificador, versión, propósito, principios, no objetivos, criterios de éxito, permisos máximos y política de aprobación. Cada ejecución fija la versión aprobada de la Constitución, el Outcome Contract y sus Execution Contracts. Un cambio de alcance o permisos requiere una nueva ejecución con contratos revisados. Un reintento conserva las versiones fijadas.

## Outcome Contract

```yaml
id: outcome.example
version: 1
title: Resultado observable
intent: Qué cambio debe existir al finalizar
scope:
  included: []
  excluded: []
acceptance:
  - id: AC-001
    statement: Criterio verificable
    evidence: [test-report]
phases: [phase.design, phase.implement, phase.verify]
risk_class: medium
approval_mode: explicit
```

## Execution Contract

```yaml
phase_id: phase.implement
executor: codex-cli
model_policy:
  preferred: configurable
  fallback: configurable
  switch_conditions: [provider_unavailable, policy_threshold]
  on_unknown: block
auth_mode: subscription_session
workspace:
  isolation: git_worktree
  base_ref: main
permissions: [read_repo, write_worktree, run_tests]
limits:
  timeout_minutes: 30
  max_attempts: 2
evidence_required: [diff, test-report, execution-summary]
verification:
  independent: true
  required_status: pass
```

## Estados de ejecución

`draft → ready → running → verifying → succeeded` ilustra el recorrido exitoso de una ejecución de una sola fase.

Las transiciones, estados terminales y condiciones de recuperación se detallan en [el ciclo de vida](06-trazabilidad-estados-recuperacion.md).

Regla: un estado debe describir lo que se conoce, no lo que se espera. `running` no implica progreso real; el progreso se expresa mediante eventos y evidencia.

## Resultado de adaptador

Debe incluir como mínimo: identidad y versión del adaptador, invocación normalizada y redactada, referencia opaca a sesión si aplica, inicio y fin, exit code si existe, resumen redactado, artefactos, errores y señales de capacidad observadas. Clasificación propuesta: `completed`, `contract_rejected`, `provider_unavailable`, `timeout`, `cancelled` y `unclassified_error`. Solo se atribuye indisponibilidad al proveedor si existe evidencia. `completed` describe el intento y requiere verificación aparte.
