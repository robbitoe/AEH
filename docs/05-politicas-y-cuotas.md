# 5. Políticas, cuotas y selección de modelos

## Suscripciones frente a API

AEH debe distinguir `subscription_session` de `api_token`. Una suscripción puede autorizar el uso interactivo de una herramienta sin exponer a AEH un contador de tokens ni una API programable. La autenticación existente se referencia, pero no se copia ni se registra en texto plano.

## Modelo de capacidad

AEH construye el snapshot a partir de las señales observables:

- `known`: el proveedor expone un valor verificable;
- `estimated`: AEH calcula una aproximación con señales incompletas;
- `unknown`: no hay medición confiable;

`blocked` representa una decisión de ejecución. Se registra aparte de la medición y puede coexistir con `unknown`. Propuesta: registrar fuente, unidad, momento de observación y vigencia de cada medición. Una estimación exige explicar el método y su incertidumbre. Las estimaciones pueden orientar revisión humana, mientras que un umbral de cuota automático exige una medición verificable y vigente. Ante `unknown`, aplicar la regla explícita del contrato. Si falta, bloquear antes de invocar.

Ejemplo: si se desea cambiar de proveedor al 10% restante pero la suscripción no expone el restante, la condición no puede evaluarse automáticamente. La política debe usar una alternativa explícita: pausa, aprobación manual, señal de error o umbral operativo configurable.

## Selección por fase

La política puede elegir modelo por:

- tipo de fase: diseño, implementación, revisión, pruebas;
- riesgo y sensibilidad del repositorio;
- latencia esperada;
- disponibilidad observada;
- preferencia del usuario;
- necesidad de independencia entre productor y verificador.

La selección debe quedar registrada como decisión, con candidatos considerados y motivo final.

## Reglas iniciales

1. No cambiar de proveedor solo por una cuota desconocida sin una condición de seguridad.
2. El fallback requiere compatibilidad con el contrato y permisos equivalentes o más restrictivos.
3. Una acción de riesgo alto requiere aprobación explícita.
4. El verificador no debe reutilizar ciegamente la misma evidencia que el productor afirma.
