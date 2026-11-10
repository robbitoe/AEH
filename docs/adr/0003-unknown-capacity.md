# ADR-0003 — Capacidad desconocida como estado válido

- Estado: aceptada para MVP-0
- Fecha: 2026-11-10

## Contexto

Una suscripción Pro puede permitir uso de una herramienta sin exponer un contador fiable de tokens o cuota a un orquestador externo.

## Decisión

`unknown` es un valor explícito y no equivale a `0`, `100%` ni “disponible”. Las políticas deben definir qué hacer ante esa incertidumbre.

## Consecuencias

El dashboard debe mostrar la incertidumbre. Las reglas automáticas basadas en “10% restante” solo se activan cuando existe una señal verificable o una política alternativa aprobada.
