# Contexto do projeto (para retomar)

Última atualização: 2026-11-09

## Objetivo

Webhook em Go hospedado na Vercel que imita o "auto-fill" do Notion. Quando o
**título** ou a **Description** de uma página de uma database mudam, o webhook
pede ao Claude Haiku uma categoria e preenche o select **Category**. A ideia é
não pagar pelo recurso nativo do Notion.

## Decisões tomadas

- **Webhooks da API do Notion** (assinatura na integração), nos eventos
  `page.created` e `page.properties_updated`. A automação nativa de database
  foi descartada porque é paga.
- **Integração interna** do Notion (token `ntn_...`), sem OAuth, com as
  capabilities Read content e Update content. A database precisa estar
  compartilhada com ela (`...` > Connections).
- **Modelo:** `claude-haiku-5-5` com effort `low` e saída estruturada (JSON
  schema com `enum`), então a resposta é sempre uma das opções.
- **Categorias:** são as opções do próprio select `Category` no Notion, lidas
  do schema da data source a cada evento. Não ficam no código. A descrição de
  cada opção, se existir, vai junto para o modelo.
- **IDs das propriedades:** não precisam ser configurados. O webhook busca a
  página e descobre os IDs pelo nome (`Description`) e pelo tipo (title).
- **Anti-loop:** só processa quando `updated_properties` contém o ID do título
  ou da Description. A nossa própria escrita na Category é ignorada.
- **Cliente Notion:** HTTP próprio e mínimo, com `Notion-Version: 2026-03-11`.
- **Retry:** 429/5xx/timeout respondem 503 (o Notion reenvia). Erros de config
  ou resposta ruim do modelo respondem 200 e só logam.
- **Pausa:** `WEBHOOK_PAUSED=true` responde 200 e descarta os eventos (eles se
  perdem). Na Vercel, só vale depois de um redeploy.
- `go 1.25` no `go.mod`, a versão mínima do SDK, por compatibilidade com a Vercel.

## Estrutura

- `api/webhook.go`: entrypoint da Vercel Function (`Handler`).
- `pkg/webhook/handler.go`: lógica (assinatura, filtros, retry, pausa) e testes.
- `pkg/classifier/classifier.go`: chamada ao Haiku (`anthropic-sdk-go/v2` v2.7.0).
- `pkg/notion/client.go`: GetPage, GetDataSource, SetSelect.
- `cmd/local/main.go`: servidor local (para uso com ngrok).
- `README.md`, `.env.example`, `.vercelignore`.

## Estado atual

- Projeto Vercel: `thallyssonkleins-projects/categorize-tasks`, preset "Other"
  (normal: a função em `/api` é detectada sozinha).
- **URL do webhook:** `https://categorize-tasks.vercel.app/api/webhook`.
  Não usar a URL com hash do deploy, que é protegida por login da Vercel.
- O deploy em produção está funcionando (GET 405, POST 200).
- Na Vercel já estão configuradas `NOTION_TOKEN` e `ANTHROPIC_API_KEY`.
- A assinatura do webhook foi **criada no Notion, mas ainda não verificada**.
  Enquanto estiver pendente, nenhum evento chega.
- O `verification_token` chegou e está nos logs da Vercel (2026-11-08 ~22:35
  UTC). Para recuperar:
  `vercel logs --environment production --since 24h --no-branch --expand -q verification_token`
  (logs antigos podem expirar; nesse caso use "Resend token" no Notion).
- **Ainda não está no ar:** o código da pausa (`WEBHOOK_PAUSED`), adicionado
  depois do último deploy.
- Nada foi commitado no git ainda.

## Próximos passos

1. Se quiser começar pausado: `vercel env add WEBHOOK_PAUSED production` (true).
2. `vercel env add NOTION_WEBHOOK_SECRET production` com o verification_token.
3. `vercel --prod` (sobe o código da pausa e passa a validar as assinaturas).
4. No Notion, clicar em **Verify** na assinatura e colar o token.
5. Teste real: editar a Description de uma página, esperar ~1 min e olhar os
   logs (`vercel logs --environment production --since 10m --no-branch --expand`).
   - Esperado: `page ... "<título>" -> "<categoria>"`.
   - Se aparecer `skipped: title and description unchanged` depois de editar a
     descrição, a comparação de IDs falhou (evento vs. API, `%3A` vs `:`), e é
     preciso ajustar `touches()` em `pkg/webhook/handler.go`.
6. Opcional: definir `NOTION_DATABASE_ID` para restringir a uma database.
7. Commitar o projeto.

## Pontos ainda não validados

- A chamada real ao Haiku com structured output (não havia API key local).
- Se o formato dos IDs em `updated_properties` bate com o da API (ver passo 5).
