# 8. Riesgos y pendientes

| ID | Riesgo / pregunta | Impacto | Mitigación o validación |
|---|---|---|---|
| R-01 | Suscripciones no exponen cuota programable | Alto | Tratar capacidad como `unknown`; probar señales permitidas por cada CLI |
| R-02 | Cambios incompatibles en CLIs | Alto | Adaptadores versionados, smoke tests y detección de capacidades |
| R-03 | Exposición de credenciales en logs | Alto | Redacción, referencias a sesión y pruebas negativas |
| R-04 | Agente declara éxito sin evidencia | Alto | Contratos con evidencia obligatoria y verificador independiente |
| R-05 | Sandbox insuficiente | Alto | Threat model, permisos mínimos y pruebas de escape/limpieza |
| R-06 | Progreso engañoso | Medio | Separar porcentaje declarado de criterios cumplidos |
| R-07 | Reintentos duplican efectos | Medio | Idempotencia, checkpoints y políticas de compensación |
| R-08 | Coste/latencia impredecibles | Medio | Límites por fase, pausas y selección configurable |

## Pendientes de validar

- Qué interfaces públicas ofrecen exactamente Codex CLI, la app de ChatGPT y Claude Code para ejecución supervisada.
- Qué señales de sesión, límites o errores pueden observarse sin API token.
- Si el entorno objetivo será local, servidor personal o despliegue compartido.
- Si PostgreSQL es necesario desde MVP-1 o puede posponerse detrás de una abstracción.
- Qué acciones requieren aprobación por riesgo y cuáles pueden automatizarse.
- Qué verificador mínimo se acepta para declarar éxito.
- Qué modelos locales se consideran disponibles y bajo qué hardware/licencia.
