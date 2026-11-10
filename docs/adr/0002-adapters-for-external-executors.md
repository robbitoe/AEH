# ADR-0002 — Adaptadores para ejecutores externos

- Estado: aceptada para MVP-0
- Fecha: 2026-11-10

## Contexto

Codex CLI y Claude Code pueden cambiar sus interfaces y sus modelos de autenticación. AEH no debe acoplar el dominio a un proveedor.

## Decisión

Cada ejecutor se integra mediante un adaptador con contrato normalizado. El adaptador no decide políticas de negocio ni gestiona secretos por defecto.

## Consecuencias

Se necesita una capa de capacidades y pruebas por versión. A cambio, el núcleo puede trabajar con un simulador y con proveedores distintos.
