# ADR-0006 — Registro de ejecución y evidencia

Estado: propuesta

## Contexto

La recuperación y la verificación deben referirse a los mismos contratos y artefactos que originaron cada decisión.

## Decisión propuesta

Fijar versiones inmutables de contratos al preparar una ejecución. Rechazar versiones incompatibles antes de iniciar un intento. Registrar transiciones y eventos juntos, con secuencia creciente por ejecución. Conservar resultados de intentos anteriores al reintentar.

Cada Evidence identifica criterio, ejecución, fase, intento, origen, tipo de artefacto, referencia, fecha y huella del contenido conservado. Redactar antes de guardar. Si no se puede conservar evidencia utilizable de manera segura, registrar la omisión y enviar a revisión cuando impida verificar un criterio.

En MVP-1, un verificador determinista separado del simulador comprueba el artefacto y emite un dictamen propio por criterio. Un artefacto ausente produce `needs_review`. Una comprobación ejecutada que incumple el criterio produce `fail`. Un dictamen humano requiere registrar identidad, evidencia revisada y motivo.

## Consecuencias

La secuencia y las huellas sirven para comprobar orden y correspondencia. Pendiente definir retención, purga autorizada e integridad frente a manipulación antes de usar evidencia real. La independencia exigida por clase de riesgo queda pendiente antes de habilitar ejecuciones supervisadas.
