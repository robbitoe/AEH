# AEH — Autonomous Engineering Harness

AEH es un meta-harness orientado a outcomes para coordinar trabajo de ingeniería ejecutado por agentes y herramientas externas, con contratos explícitos, políticas, trazabilidad y verificación independiente.

## Estado actual

**Fase: MVP-0 — documentación controlada.** No se implementan todavía backend, dashboard, adaptadores ni automatizaciones reales.

La prioridad es validar el modelo mental y cerrar decisiones antes de construir:

- qué significa “resultado logrado”;
- cómo se describe y ejecuta una fase;
- cómo se coordinan Codex CLI y Claude Code autenticados mediante suscripciones;
- cómo se expresan cuotas cuando la disponibilidad real no está expuesta;
- qué evidencia permite verificar un resultado de forma independiente;
- qué nivel de aislamiento y recuperación requiere cada ejecución.

## Navegación

- [Glosario](docs/10-glosario.md)
- [Matriz de validación](docs/11-validacion.md)

- [Visión y alcance](docs/01-vision.md)
- [Requisitos](docs/02-requisitos.md)
- [Arquitectura](docs/03-arquitectura.md)
- [Contratos de outcome y ejecución](docs/04-contratos.md)
- [Políticas, cuotas y selección de modelos](docs/05-politicas-y-cuotas.md)
- [Trazabilidad, estados y recuperación](docs/06-trazabilidad-estados-recuperacion.md)
- [Roadmap MVP](docs/07-roadmap-mvp.md)
- [Riesgos y pendientes](docs/08-riesgos-y-pendientes.md)
- [Criterios de aceptación](docs/09-criterios-de-aceptacion.md)
- [ADRs](docs/adr/README.md)

## Hipótesis centrales

El alcance inicial de MVP-1 está en revisión en la [ADR-0005](docs/adr/0005-local-baseline.md). Las capacidades de los proveedores reales quedan pendientes de validación para MVP-2.

1. Las suscripciones Pro de las herramientas no equivalen necesariamente a acceso API ni exponen una cuota programable.
2. AEH debe tratar la capacidad disponible como `unknown` cuando el proveedor no publica una medición confiable.
3. Los adaptadores deben poder funcionar con autenticación interactiva existente, sin convertir AEH en un gestor de secretos.
4. Un panel de progreso es útil, pero no debe ser requisito para que el motor ejecute y deje evidencia.
5. La verificación debe ser una responsabilidad separable del agente que produjo el cambio.

## Principio rector

> AEH no promete autonomía por apariencia; promete resultados trazables, límites explícitos y estados honestos.

## Licencia

[MIT](LICENSE).
