# Builds any plugin in plugins/ from the repository root:
#
#   docker buildx build --platform linux/arm64,linux/amd64 \
#     -f plugins/Dockerfile --build-arg PLUGIN=example-switch .
#
# PLUGIN is the directory name; its workspace must be @harrymitchinson/oikade-plugin-<PLUGIN>.
ARG NODE_IMAGE=node:22-alpine

# TypeScript compiles once on the build machine; its output is platform-independent.
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS build
ARG PLUGIN=example-switch
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
WORKDIR /src
RUN corepack enable
COPY package.json yarn.lock .yarnrc.yml tsconfig.base.json ./
COPY sdk sdk
COPY plugins plugins
RUN yarn install --immutable
RUN yarn workspace @harrymitchinson/oikade-plugin-sdk build && \
    yarn workspace @harrymitchinson/oikade-plugin-${PLUGIN} build

# Production dependencies install on the target platform, so native modules match it.
FROM ${NODE_IMAGE} AS deps
ARG PLUGIN=example-switch
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
WORKDIR /src
RUN corepack enable
COPY package.json yarn.lock .yarnrc.yml ./
COPY sdk sdk
COPY plugins plugins
RUN yarn workspaces focus --production @harrymitchinson/oikade-plugin-${PLUGIN} && \
    mkdir -p sdk/typescript/node_modules plugins/${PLUGIN}/node_modules

FROM ${NODE_IMAGE}
ARG PLUGIN=example-switch
ENV NODE_ENV=production
WORKDIR /app
COPY --from=deps /src/node_modules node_modules
COPY --from=deps /src/sdk/typescript/node_modules sdk/typescript/node_modules
COPY --from=deps /src/plugins/${PLUGIN}/node_modules plugins/${PLUGIN}/node_modules
COPY --from=build /src/sdk/typescript/package.json sdk/typescript/
COPY --from=build /src/sdk/typescript/dist sdk/typescript/dist
COPY --from=build /src/plugins/${PLUGIN}/package.json plugins/${PLUGIN}/
COPY --from=build /src/plugins/${PLUGIN}/dist plugins/${PLUGIN}/dist
# A fixed path, so CMD can stay in exec form and node receives SIGTERM directly.
RUN ln -s plugins/${PLUGIN} plugin
# The node user, matching the operator's pod security context.
USER 1000:1000
CMD ["node", "/app/plugin/dist/main.js"]
