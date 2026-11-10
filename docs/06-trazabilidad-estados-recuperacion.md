# 6. Trazabilidad, estados y recuperación

## Ciclo de vida propuesto

| Origen | Destino | Condición |
|---|---|---|
| `draft` | `ready` | El coordinador valida los contratos y fija sus versiones. |
| `ready` | `running`, `blocked` | El coordinador inicia si la política lo permite. Si falta un requisito o aprobación, bloquea y registra el motivo. |
| `running` | `verifying`, `paused`, `blocked`, `failed` | El coordinador registra evidencia del intento, una interrupción confirmada, un bloqueo de política o un fallo sin recuperación autorizada, respectivamente. |
| `verifying` | `ready`, `succeeded`, `needs_review`, `failed` | El verificador emite dictamen. Con `pass`, el coordinador habilita la siguiente fase o cierra el outcome si todos los criterios pasan. La evidencia insuficiente y el fallo recuperable van a revisión. Un fallo irrecuperable termina en `failed`. |
| `paused`, `blocked`, `needs_review` | `ready`, `failed` | El operador registra una resolución. El coordinador revalida permisos, evidencia y checkpoint antes de volver a `ready`. Si la recuperación es inviable, registra la causa. |
| Cualquier estado no terminal | `cancelled` | El operador autorizado solicita cancelación. Si hay trabajo activo, el coordinador confirma su detención antes de cerrar. |

`succeeded`, `failed` y `cancelled` son terminales. Para reintentar un fallo recuperable, el coordinador registra el fallo del intento y pausa para reevaluar. El reintento y el fallback consumen el mismo presupuesto de intentos de la fase. Si el efecto previo es incierto o el checkpoint es inválido, se requiere revisión antes de ejecutar de nuevo.

## Registro mínimo

Cada transición debe registrar: ejecución, fase, estado anterior, estado nuevo, actor, timestamp, motivo, política aplicada, modelo/adaptador, referencia de workspace y evidencia asociada.

## Eventos

Los eventos son append-only a nivel lógico. Los resúmenes pueden recalcularse, pero no deben sustituir el historial original. El dashboard puede consumir un stream o consultar snapshots derivados.

## Checkpoints

Un checkpoint representa el último punto desde el cual una fase puede reintentarse sin duplicar efectos peligrosos. Debe incluir commit/base, artefactos aceptados, variables no secretas y estado del contrato.

## Recuperación

- **Reintento:** misma fase y contrato, con intento nuevo.
- **Fallback:** nuevo adaptador/modelo autorizado por política.
- **Pausa:** conserva evidencia y requiere decisión posterior.
- **Escalado:** deriva a revisión humana o fase especializada.
- **Compensación:** revierte o aísla cambios cuando el contrato lo permita.

La recuperación no debe ocultar el intento fallido ni sobrescribir su evidencia.
