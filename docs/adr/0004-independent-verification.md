# ADR-0004 — Verificación independiente

- Estado: aceptada para MVP-0
- Fecha: 2026-11-10

## Contexto

El mismo agente que produce un cambio puede interpretar sus propios criterios de forma optimista.

## Decisión

La verificación es una etapa separable, con evidencia y resultado propios. Puede ser determinista, ejecutada por otro modelo/adaptador o realizada por una persona.

## Consecuencias

Las ejecuciones deben conservar evidencia de producción y verificación por separado. La independencia será gradual en el MVP, pero el contrato debe existir desde el inicio.
