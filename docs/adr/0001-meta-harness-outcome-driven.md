# ADR-0001 — AEH como meta-harness orientado a outcomes

- Estado: aceptada para MVP-0
- Fecha: 2026-11-10

## Contexto

Coordinar agentes por comandos aislados no ofrece suficiente control sobre intención, evidencia, límites ni recuperación.

## Decisión

AEH se diseña como una capa de orquestación orientada a outcomes, no como un agente único ni como un simple lanzador de procesos.

## Consecuencias

Se requieren contratos, estados, eventos, políticas y verificación. El diseño inicial es más amplio, pero permite cambiar ejecutores sin cambiar la intención del producto.
