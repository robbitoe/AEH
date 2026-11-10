# 3. Arquitectura propuesta

El diagrama representa la arquitectura objetivo. La [ADR-0005](adr/0005-local-baseline.md) propone una base local simulada para MVP-1 y deja pendiente la tecnología de persistencia.

```text
Usuario
  │
  ▼
Dashboard React ──► API ASP.NET Core ──► Orchestrator
                         │                 ├─ Policy Engine
                         │                 ├─ Model/Capacity Resolver
                         │                 ├─ Execution Coordinator
                         │                 └─ Verification Coordinator
                         ▼
                    PostgreSQL

Execution Coordinator ─► Worktree/Sandbox Manager
                       ├► Codex CLI Adapter
                       ├► Claude Code Adapter
                       └► Optional adapters

Todos producen: eventos, logs resumidos, artefactos y evidencia verificable.
```

## Componentes

### Product Constitution

Documento versionado que limita objetivos, riesgo aceptable, permisos y criterios de éxito. Es una entrada de diseño, no una instrucción libre para el agente.

### Orchestrator

Mantiene el grafo de fases, evalúa precondiciones, solicita adaptador/modelo, aplica políticas y coordina pausas, reintentos y escalados.

### Policy Engine

Evalúa permisos, acciones de riesgo, límites de tiempo, dependencia de evidencia, necesidad de aprobación y reglas de fallback.

### Capacity Resolver

Normaliza señales de disponibilidad de cada proveedor. Distingue cuota confirmada, estimación local y valor desconocido.

### Adapters

Traducen un `Execution Contract` a una invocación concreta. No deben contener lógica de negocio ni decidir por sí solos cuándo cambiar de proveedor.

### Verification Coordinator

Ejecuta pruebas, análisis o revisión independiente y asocia sus resultados al outcome. Puede usar un modelo distinto, herramientas deterministas o una persona.

### Dashboard

Presenta estado, fase activa, progreso declarado, eventos, decisiones, evidencia, bloqueos y acciones autorizadas. No es la fuente de verdad: la fuente es la API y su historial persistido.

## Flujo de alto nivel

1. Se registra la Constitución y el outcome.
2. Se valida el contrato y se crea una ejecución aislada.
3. El orquestador selecciona fase, adaptador y modelo según política.
4. El adaptador ejecuta y devuelve resultado más evidencia.
5. El verificador evalúa criterios.
6. Se persiste la decisión: avanzar, pausar, reintentar, escalar o fallar.
7. El dashboard refleja el estado y la trazabilidad.

## Persistencia inicial

Entidades previstas: `ProductConstitution`, `Outcome`, `Phase`, `Execution`, `ExecutionAttempt`, `PolicyDecision`, `Event`, `Artifact`, `Verification`, `ProviderCapabilitySnapshot`.
