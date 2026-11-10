# 1. Visión y alcance

## Problema

Los agentes de ingeniería pueden ejecutar tareas complejas, pero el usuario necesita controlar intención, alcance, herramientas, modelos, consumo, aislamiento, progreso, evidencia y recuperación. Las suscripciones de Codex CLI, la app de ChatGPT y Claude Code pueden ofrecer experiencias distintas de las APIs basadas en tokens; AEH debe coordinar esas superficies sin inventar capacidades que no estén expuestas.

## Visión

AEH convierte una intención de producto en una secuencia de outcomes verificables. Un orquestador selecciona un ejecutor y modelo por fase, aplica políticas, ejecuta en un worktree/sandbox, recoge evidencia y solicita verificación independiente antes de avanzar o declarar un resultado.

## Dentro del alcance

- Product Constitution como fuente de principios, límites y criterios de éxito.
- Descomposición de un outcome en fases.
- Contratos de outcome y de ejecución.
- Orquestación de adaptadores para Codex CLI y Claude Code.
- Selección configurable de modelos por fase.
- Capacidad con estados `known`, `estimated` y `unknown`, y bloqueo como decisión de ejecución.
- Dashboard React de estado, eventos y evidencia.
- Backend ASP.NET Core y PostgreSQL.
- Git worktrees/sandboxes, políticas, recuperación y trazabilidad.

## Fuera del alcance inicial

- Automatización de login, evasión de límites o scraping de cuotas.
- Garantía de disponibilidad o de tokens de proveedores de suscripción.
- Ejecución autónoma sin límites de tiempo, costo, permisos o revisión.
- Multi-tenant, facturación y despliegue cloud como requisitos del MVP.
- Reemplazar la interfaz o política propia de Codex/Claude.

## Usuarios primarios

- Persona técnica que define y supervisa outcomes.
- Operador que inspecciona ejecuciones, pausas y recuperación.
- Verificador que revisa evidencia y criterios.
