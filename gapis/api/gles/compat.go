// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gles

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/gapid/core/data/deep"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/math/interval"
	"github.com/google/gapid/core/math/u32"
	"github.com/google/gapid/core/math/u64"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/gles/glsl/ast"
	"github.com/google/gapid/gapis/api/gles/glsl/preprocessor"
	"github.com/google/gapid/gapis/api/transform"
	"github.com/google/gapid/gapis/config"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/replay"
	"github.com/google/gapid/gapis/replay/builder"
	"github.com/google/gapid/gapis/shadertools"
)

type support int

const (
	unsupported support = iota
	supported
	required
)

var (
	// We don't include tests directly in the gles package as it adds
	// signaficantly to the test build time.
	VisibleForTestingCompat     = compat
	VisibleForTestingGlSlCompat = glslCompat
)

// If the default vertex array object (id 0) is not allowed on
// the target platform, we remap the uses to this array.
const DefaultVertexArrayId = VertexArrayId(0xFFFF0001)

type extensions map[string]struct{}

func listToExtensions(list []string) extensions {
	out := extensions{}
	for _, s := range list {
		out[s] = struct{}{}
	}
	return out
}

func translateExtensions(in U32ːstringᵐ) extensions {
	out := make(extensions, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

func (e extensions) get(name string) support {
	if _, ok := e[name]; ok {
		return supported
	}
	return unsupported
}

func (s support) String() string {
	switch s {
	case unsupported:
		return "unsupported"
	case supported:
		return "supported"
	case required:
		return "required"
	default:
		return fmt.Sprintf("support<%d>", s)
	}
}

type features struct {
	vertexHalfFloatOES        support // support for GL_OES_vertex_half_float
	eglImageExternal          support // support for GL_OES_EGL_image_external
	textureMultisample        support // support for ARB_texture_multisample
	vertexArrayObjects        support // support for VBOs
	supportGenerateMipmapHint bool    // support for GL_GENERATE_MIPMAP_HINT
	compressedTextureFormats  map[GLenum]struct{}
	framebufferSrgb           support // support for GL_FRAMEBUFFER_SRGB
}

func getFeatures(ctx context.Context, version string, ext extensions) (features, *Version, error) {
	v, err := ParseVersion(version)
	if err != nil {
		return features{}, v, err
	}

	f := features{
		vertexHalfFloatOES:        ext.get("GL_OES_vertex_half_float"),
		eglImageExternal:          ext.get("GL_OES_EGL_image_external"),
		textureMultisample:        ext.get("ARB_texture_multisample"),
		compressedTextureFormats:  getSupportedCompressedTextureFormats(ext),
		supportGenerateMipmapHint: v.IsES,
	}

	// TODO: Properly check the specifications for these flags.
	switch {
	case v.AtLeastES(3, 0):
		f.vertexArrayObjects = supported
	case v.AtLeastGL(3, 0):
		f.vertexArrayObjects = required
	}

	if v.AtLeastES(3, 2) {
		f.textureMultisample = required
	}

	// GLES defaults FRAMEBUFFER_SRGB to enabled and only allows changing it via
	// an extension, while desktop GL defaults to disabled.
	if v.IsES {
		f.framebufferSrgb = ext.get("GL_EXT_sRGB_write_control")
	} else {
		f.framebufferSrgb = required
	}

	return f, v, nil
}

type scratchBuffer struct {
	size GLsizeiptr
	id   BufferId
}

func compat(ctx context.Context, device *device.Instance) (transform.Transformer, error) {
	ctx = log.Enter(ctx, "compat")

	glDev := device.Configuration.Drivers.OpenGL
	target, version, err := getFeatures(ctx, glDev.Version, listToExtensions(glDev.Extensions))
	if err != nil {
		return nil, fmt.Errorf(
			"Error '%v' when getting feature list for version: '%s', extensions: '%s'.",
			err, glDev.Version, glDev.Extensions)
	}

	contexts := map[*Context]features{}

	scratchBuffers := map[interface{}]scratchBuffer{}
	nextBufferID := BufferId(0xffff0000)
	newBuffer := func(i api.CmdID, cb CommandBuilder, out transform.Writer) BufferId {
		s := out.State()
		id := nextBufferID
		tmp := s.AllocDataOrPanic(ctx, id)
		out.MutateAndWrite(ctx, i.Derived(), cb.GlGenBuffers(1, tmp.Ptr()).AddWrite(tmp.Data()))
		nextBufferID--
		return id
	}

	nextTextureID := TextureId(0xffff0000)
	newTexture := func(i api.CmdID, cb CommandBuilder, out transform.Writer) TextureId {
		s := out.State()
		id := nextTextureID
		tmp := s.AllocDataOrPanic(ctx, id)
		out.MutateAndWrite(ctx, i.Derived(), cb.GlGenTextures(1, tmp.Ptr()).AddWrite(tmp.Data()))
		nextTextureID--
		return id
	}

	// Definitions of Vertex Arrays backed by client memory.
	// We postpone the write of the command until draw call.
	clientVAs := map[*VertexAttributeArray]*GlVertexAttribPointer{}

	textureCompat := &textureCompat{
		f: target,
		v: version,
		origSwizzle: map[GLenum]map[*Texture]GLenum{
			GLenum_GL_TEXTURE_SWIZZLE_R: {},
			GLenum_GL_TEXTURE_SWIZZLE_G: {},
			GLenum_GL_TEXTURE_SWIZZLE_B: {},
			GLenum_GL_TEXTURE_SWIZZLE_A: {},
		},
		compatSwizzle: map[*Texture]map[GLenum]GLenum{},
	}

	// TODO: Implement full support for external images.
	convertTexTarget := func(t *GLenum) {
		if *t == GLenum_GL_TEXTURE_EXTERNAL_OES && target.eglImageExternal == unsupported {
			// Remap external textures to plain 2D textures - this matches GLSL compat.
			// TODO: This aliases GLenum_GL_TEXTURE_EXTERNAL_OES and GLenum_GL_TEXTURE_2D
			*t = GLenum_GL_TEXTURE_2D
		}
	}

	var t transform.Transformer
	t = transform.Transform("compat", func(ctx context.Context, id api.CmdID, cmd api.Cmd, out transform.Writer) {
		dID := id.Derived()
		s := out.State()
		cb := CommandBuilder{Thread: cmd.Thread()}
		switch cmd := cmd.(type) {
		case *EglMakeCurrent: // TODO: Check for GLX, CGL, WGL...
			// The compatibility layer introduces calls to GL functions that are defined for desktop GL
			// and for GLES 3.0+. If the trace originated on a GLES 2.0 device, these new atoms' mutate
			// functions will fail the minRequiredVersion checks (which look at the version coming from
			// the original context from the trace).
			// TODO(dsrbecky): This might make some atoms valid for replay which were invalid on trace.
			scs := FindStaticContextState(cmd.Extras())
			if scs != nil && !version.IsES && scs.Constants.MajorVersion < 3 {
				clone, err := deep.Clone(cmd)
				if err != nil {
					panic(err)
				}
				cmd = clone.(*EglMakeCurrent)
				for _, e := range cmd.extras.All() {
					if cs, ok := e.(*StaticContextState); ok {
						cs.Constants.MajorVersion = 3
						cs.Constants.MinorVersion = 0
					}
				}
			}

			// Mutate to set the context, Version and Extensions strings.
			out.MutateAndWrite(ctx, id, cmd)

			c := GetContext(s, cmd.Thread())
			if c == nil || !c.Info.Initialized {
				return
			}
			if _, found := contexts[c]; found {
				return
			}

			source, _, err := getFeatures(ctx, c.Constants.Version, translateExtensions(c.Constants.Extensions))
			if err != nil {
				log.E(log.V{
					"version":    c.Constants.Version,
					"extensions": c.Constants.Extensions,
				}.Bind(ctx), "Error getting feature list: %v", err)
				return
			}

			contexts[c] = source

			if target.vertexArrayObjects == required &&
				source.vertexArrayObjects != required {
				// Replay device requires VAO, but capture did not enforce it.
				// Satisfy the target by creating and binding a single VAO
				// which we will use instead of the default VAO (id 0).
				tmp := s.AllocDataOrPanic(ctx, VertexArrayId(DefaultVertexArrayId))
				out.MutateAndWrite(ctx, dID, cb.GlGenVertexArrays(1, tmp.Ptr()).AddWrite(tmp.Data()))
				out.MutateAndWrite(ctx, dID, cb.GlBindVertexArray(DefaultVertexArrayId))
			}
			return
		}

		c := GetContext(s, cmd.Thread())
		if c == nil || !c.Info.Initialized {
			// The compatibility translations below assume that we have a valid context.
			out.MutateAndWrite(ctx, id, cmd)
			return
		}

		switch cmd := cmd.(type) {
		case *GlBindBuffer:
			if cmd.Buffer != 0 && !c.Objects.Shared.GeneratedNames.Buffers[cmd.Buffer] {
				// glGenBuffers() was not used to generate the buffer. Legal in GLES 2.
				tmp := s.AllocDataOrPanic(ctx, cmd.Buffer)
				out.MutateAndWrite(ctx, dID, cb.GlGenBuffers(1, tmp.Ptr()).AddRead(tmp.Data()))
			}

		case *GlBindTexture:
			{
				cmd := *cmd
				if cmd.Texture != 0 && !c.Objects.Shared.GeneratedNames.Textures[cmd.Texture] {
					// glGenTextures() was not used to generate the texture. Legal in GLES 2.
					tmp := s.AllocDataOrPanic(ctx, VertexArrayId(cmd.Texture))
					out.MutateAndWrite(ctx, dID, cb.GlGenTextures(1, tmp.Ptr()).AddRead(tmp.Data()))
				}

				convertTexTarget(&cmd.Target)

				out.MutateAndWrite(ctx, id, &cmd)
				return
			}

		case *GlBindVertexArray:
			if cmd.Array == VertexArrayId(0) {
				if target.vertexArrayObjects == required &&
					contexts[c].vertexArrayObjects != required {
					// NB: This leaks state change upstream.
					// In particular, when the tweaker saves and then restores vertex array binding,
					// it will restore it to DefaultVertexArrayId instead of 0.  It is harmless.
					out.MutateAndWrite(ctx, id, cb.GlBindVertexArray(DefaultVertexArrayId))
					return
				}
			}

		case *GlBindVertexArrayOES:
			if cmd.Array == VertexArrayId(0) {
				if target.vertexArrayObjects == required &&
					contexts[c].vertexArrayObjects != required {
					out.MutateAndWrite(ctx, id, cb.GlBindVertexArray(DefaultVertexArrayId))
					return
				}
			}

		case *GlBindBufferRange:
			misalignment := cmd.Offset % GLintptr(glDev.UniformBufferAlignment)
			if cmd.Target == GLenum_GL_UNIFORM_BUFFER && misalignment != 0 {
				// We have a glBindBufferRange() taking a uniform buffer with an
				// illegal offset alignment.
				// TODO: We don't handle the case where the buffer is kept bound
				// while the buffer is updated. It's an unlikely issue, but
				// something that may break us.
				if _, ok := c.Objects.Shared.Buffers[cmd.Buffer]; !ok {
					return // Don't know what buffer this is referring to.
				}

				// We need a scratch buffer to copy the buffer data to a correct
				// alignment.
				key := struct {
					c      *Context
					Target GLenum
					Index  GLuint
				}{c, cmd.Target, cmd.Index}

				// Look for pre-existing buffer we can reuse.
				buffer, ok := scratchBuffers[key]
				if !ok {
					buffer.id = newBuffer(dID, cb, out)
					scratchBuffers[key] = buffer
				}

				// Bind the scratch buffer to GL_COPY_WRITE_BUFFER
				origCopyWriteBuffer := c.Bound.CopyWriteBuffer
				out.MutateAndWrite(ctx, dID, cb.GlBindBuffer(GLenum_GL_COPY_WRITE_BUFFER, buffer.id))

				if buffer.size < cmd.Size {
					// Resize the scratch buffer
					out.MutateAndWrite(ctx, dID, cb.GlBufferData(GLenum_GL_COPY_WRITE_BUFFER, cmd.Size, memory.Nullptr, GLenum_GL_DYNAMIC_COPY))
					buffer.size = cmd.Size
					scratchBuffers[key] = buffer
				}

				// Copy out the misaligned data to the scratch buffer in the
				// GL_COPY_WRITE_BUFFER binding.
				out.MutateAndWrite(ctx, dID, cb.GlBindBuffer(cmd.Target, cmd.Buffer))
				out.MutateAndWrite(ctx, dID, cb.GlCopyBufferSubData(cmd.Target, GLenum_GL_COPY_WRITE_BUFFER, cmd.Offset, 0, cmd.Size))

				// We can now bind the range with correct alignment.
				out.MutateAndWrite(ctx, id, cb.GlBindBufferRange(cmd.Target, cmd.Index, buffer.id, 0, cmd.Size))

				// Restore old GL_COPY_WRITE_BUFFER binding.
				out.MutateAndWrite(ctx, dID, cb.GlBindBuffer(GLenum_GL_COPY_WRITE_BUFFER, origCopyWriteBuffer.GetID()))

				return
			}

		case *GlDisableVertexAttribArray:
			if c.Bound.VertexArray.VertexAttributeArrays[cmd.Location].Enabled == GLboolean_GL_FALSE {
				// Ignore the call if it is redundant (i.e. it is already disabled).
				// Some applications iterate over all arrays and explicitly disable them.
				// This is a problem if the target supports fewer arrays than the capture.
				return
			}

		case *GlVertexAttrib4fv:
			if oldAttrib, ok := c.Vertex.Attributes[cmd.Location]; ok {
				oldValue := oldAttrib.Value.Read(ctx, cmd, s, nil /* builder */)
				cmd.Mutate(ctx, s, nil /* no builder, just mutate */)
				newAttrib := c.Vertex.Attributes[cmd.Location]
				newValue := newAttrib.Value.Read(ctx, cmd, s, nil /* builder */)
				if reflect.DeepEqual(oldValue, newValue) {
					// Ignore the call if it is redundant.
					// Some applications iterate over all arrays and explicitly initialize them.
					// This is a problem if the target supports fewer arrays than the capture.
					return
				}
			}
			out.MutateAndWrite(ctx, id, cmd)
			return

		case *GlGetVertexAttribIiv,
			*GlGetVertexAttribIuiv,
			*GlGetVertexAttribPointerv,
			*GlGetVertexAttribfv,
			*GlGetVertexAttribiv:
			// Some applications iterate over all arrays and query their state.
			// This may fail if the target supports fewer arrays than the capture.
			// As these should have no side-effects, just drop them.
			return

		case *GlShaderSource:
			// Apply the state mutation of the unmodified glShaderSource atom.
			// This is so we can grab the source string from the Shader object.
			if err := cmd.Mutate(ctx, s, nil /* no builder, just mutate */); err != nil {
				return
			}
			shader := c.Objects.Shared.Shaders.Get(cmd.Shader)
			src := ""

			if config.UseGlslang {
				opts := shadertools.Option{
					IsFragmentShader: shader.Type == GLenum_GL_FRAGMENT_SHADER,
					IsVertexShader:   shader.Type == GLenum_GL_VERTEX_SHADER,
				}

				res, err := shadertools.ConvertGlsl(shader.Source, &opts)
				if err != nil {
					log.E(ctx, "glShaderSource() compat: %v", err)
					return
				}

				src = res.SourceCode
			} else {
				lang := ast.LangVertexShader
				switch shader.Type {
				case GLenum_GL_VERTEX_SHADER:
				case GLenum_GL_FRAGMENT_SHADER:
					lang = ast.LangFragmentShader
				default:
					log.W(ctx, "Unknown shader type: %v", shader.Type)
				}

				exts := []preprocessor.Extension{}
				if target.textureMultisample == supported {
					// TODO: Check that this extension is actually used by the shader.
					exts = append(exts, preprocessor.Extension{
						Name: "GL_ARB_texture_multisample", Behaviour: "enable",
					})
				}

				src, err = glslCompat(ctx, shader.Source, lang, exts, device)
				if err != nil {
					log.E(ctx, "Error reformatting GLSL source for command %d: %v", id, err)
				}
			}

			tmpSrc := s.AllocDataOrPanic(ctx, src)
			tmpPtrToSrc := s.AllocDataOrPanic(ctx, tmpSrc.Ptr())
			cmd = cb.GlShaderSource(cmd.Shader, 1, tmpPtrToSrc.Ptr(), memory.Nullptr).
				AddRead(tmpSrc.Data()).
				AddRead(tmpPtrToSrc.Data())
			out.MutateAndWrite(ctx, id, cmd)
			tmpPtrToSrc.Free()
			tmpSrc.Free()
			return

		// TODO: glVertexAttribIPointer
		case *GlVertexAttribPointer:
			if cmd.Type == GLenum_GL_HALF_FLOAT_OES && target.vertexHalfFloatOES == unsupported {
				// Convert GL_HALF_FLOAT_OES to GL_HALF_FLOAT_ARB.
				cmd = cb.GlVertexAttribPointer(cmd.Location, cmd.Size, GLenum_GL_HALF_FLOAT_ARB, cmd.Normalized, cmd.Stride, memory.Pointer(cmd.Data))
			}
			vaa := c.Bound.VertexArray.VertexAttributeArrays[cmd.Location]
			if target.vertexArrayObjects == required && c.Bound.ArrayBuffer == nil {
				// Client-pointers are not supported, we need to copy this data to a buffer.
				// However, we can't do this now as the observation only happens at the draw call.
				clientVAs[vaa] = cmd
			} else {
				delete(clientVAs, vaa)
				out.MutateAndWrite(ctx, id, cmd)
			}
			return

		case *GlDrawArrays:
			if target.vertexArrayObjects == required {
				if clientVAsBound(c, clientVAs) {
					first := uint32(cmd.FirstIndex)
					count := uint32(cmd.IndicesCount)
					t := newTweaker(out, dID, cb)
					defer t.revert(ctx)
					moveClientVBsToVAs(ctx, t, clientVAs, first, count, id, cmd, s, c, out)
				}
			}
			MultiviewDraw(ctx, id, cmd, out)
			return

		case *GlDrawElements:
			if target.vertexArrayObjects == required {
				e := externs{ctx: ctx, cmd: cmd, s: s}
				t := newTweaker(out, dID, cb)
				defer t.revert(ctx)

				ib := c.Bound.VertexArray.ElementArrayBuffer
				clientIB := ib == nil
				clientVB := clientVAsBound(c, clientVAs)
				if clientIB {
					// The indices for the glDrawElements call is in client memory.
					// We need to move this into a temporary buffer.

					// Generate a new element array buffer and bind it.
					bufID := t.glGenBuffer(ctx)
					t.GlBindBuffer_ElementArrayBuffer(ctx, bufID)

					// By moving the draw call's observations earlier, populate the element array buffer.
					size, base := DataTypeSize(cmd.IndicesType)*int(cmd.IndicesCount), memory.Pointer(cmd.Indices)
					glBufferData := cb.GlBufferData(GLenum_GL_ELEMENT_ARRAY_BUFFER, GLsizeiptr(size), memory.Pointer(base), GLenum_GL_STATIC_DRAW)
					glBufferData.extras = cmd.extras
					out.MutateAndWrite(ctx, dID, glBufferData)

					if clientVB {
						// Some of the vertex arrays for the glDrawElements call is in
						// client memory and we need to move this into temporary buffer(s).
						// The indices are also in client memory, so we need to apply the
						// atom's reads now so that the indices can be read from the
						// application pool.
						cmd.Extras().Observations().ApplyReads(s.Memory[memory.ApplicationPool])
						indexSize := DataTypeSize(cmd.IndicesType)
						data := U8ᵖ(cmd.Indices).Slice(0, uint64(indexSize*int(cmd.IndicesCount)), s.MemoryLayout)
						limits := e.calcIndexLimits(data, indexSize)
						moveClientVBsToVAs(ctx, t, clientVAs, limits.First, limits.Count, id, cmd, s, c, out)
					}

					cmd := *cmd
					cmd.Indices.addr = 0
					MultiviewDraw(ctx, id, &cmd, out)
					return

				} else if clientVB { // GL_ELEMENT_ARRAY_BUFFER is bound
					// Some of the vertex arrays for the glDrawElements call is in
					// client memory and we need to move this into temporary buffer(s).
					// The indices are server-side, so can just be read from the internal
					// pooled buffer.
					data := ib.Data
					indexSize := DataTypeSize(cmd.IndicesType)
					start := u64.Min(cmd.Indices.addr, data.count)                               // Clamp
					end := u64.Min(start+uint64(indexSize)*uint64(cmd.IndicesCount), data.count) // Clamp
					limits := e.calcIndexLimits(data.Slice(start, end, s.MemoryLayout), indexSize)
					moveClientVBsToVAs(ctx, t, clientVAs, limits.First, limits.Count, id, cmd, s, c, out)
				}
			}
			MultiviewDraw(ctx, id, cmd, out)
			return

		case *GlCompressedTexImage2D:
			if _, supported := target.compressedTextureFormats[cmd.Internalformat]; !supported {
				if err := decompressTexImage2D(ctx, id, cmd, s, out); err == nil {
					return
				}
				log.E(ctx, "Error decompressing texture: %v", err)
			}

		case *GlCompressedTexSubImage2D:
			if _, supported := target.compressedTextureFormats[cmd.Internalformat]; !supported {
				if err := decompressTexSubImage2D(ctx, id, cmd, s, out); err == nil {
					return
				}
				log.E(ctx, "Error decompressing texture: %v", err)
			}

		case *GlTexBufferEXT:
			if version.AtLeastGL(3, 1) { // Strip suffix on desktop.
				cmd := cb.GlTexBuffer(cmd.Target, cmd.Internalformat, cmd.Buffer)
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		// TODO: glTexStorage functions are not guaranteed to be supported. Consider replacing with glTexImage calls.
		// TODO: Handle glTextureStorage family of functions - those use direct state access, not the bound texture.
		case *GlTexStorage1DEXT:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					cmd := cb.GlTexStorage1D(cmd.Target, cmd.Levels, cmd.Internalformat, cmd.Width)
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage2D:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage2DEXT:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					cmd := cb.GlTexStorage2D(cmd.Target, cmd.Levels, cmd.Internalformat, cmd.Width, cmd.Height)
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage2DMultisample:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage3D:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage3DEXT:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					cmd := cb.GlTexStorage3D(cmd.Target, cmd.Levels, cmd.Internalformat, cmd.Width, cmd.Height, cmd.Depth)
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage3DMultisample:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexStorage3DMultisampleOES:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					cmd := cb.GlTexStorage3DMultisample(cmd.Target, cmd.Samples, cmd.Internalformat, cmd.Width, cmd.Height, cmd.Depth, cmd.Fixedsamplelocations)
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexImage2D:
			{
				cmd := *cmd
				internalformat := GLenum(cmd.Internalformat)
				textureCompat.convertFormat(ctx, cmd.Target, &internalformat, &cmd.Format, &cmd.Type, out, id, &cmd)
				cmd.Internalformat = GLint(internalformat)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexImage3D:
			{
				cmd := *cmd
				internalformat := GLenum(cmd.Internalformat)
				textureCompat.convertFormat(ctx, cmd.Target, &internalformat, &cmd.Format, &cmd.Type, out, id, &cmd)
				cmd.Internalformat = GLint(internalformat)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexImage3DOES:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, &cmd.Format, &cmd.Type, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					extras := cmd.extras
					cmd := cb.GlTexImage3D(cmd.Target, cmd.Level, GLint(cmd.Internalformat), cmd.Width, cmd.Height, cmd.Depth, cmd.Border, cmd.Format, cmd.Type, memory.Pointer(cmd.Pixels))
					cmd.extras = extras
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexSubImage2D:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, nil, &cmd.Format, &cmd.Type, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexSubImage3D:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, nil, &cmd.Format, &cmd.Type, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlTexSubImage3DOES:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, nil, &cmd.Format, &cmd.Type, out, id, &cmd)
				if !version.IsES { // Strip suffix on desktop.
					extras := cmd.extras
					cmd := cb.GlTexSubImage3D(cmd.Target, cmd.Level, cmd.Xoffset, cmd.Yoffset, cmd.Zoffset, cmd.Width, cmd.Height, cmd.Depth, cmd.Format, cmd.Type, memory.Pointer(cmd.Pixels))
					cmd.extras = extras
					out.MutateAndWrite(ctx, id, cmd)
					return
				}
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}
		case *GlCopyTexImage2D:
			{
				cmd := *cmd
				textureCompat.convertFormat(ctx, cmd.Target, &cmd.Internalformat, nil, nil, out, id, &cmd)
				out.MutateAndWrite(ctx, id, &cmd)
				return
			}

		case *GlTexParameterIivOES:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterIuivOES:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterIiv:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterIuiv:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterf:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Parameter, out, id, &cmd)
				return
			}
		case *GlTexParameterfv:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameteri:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Parameter, out, id, &cmd)
				return
			}
		case *GlTexParameteriv:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterIivEXT:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}
		case *GlTexParameterIuivEXT:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, id, &cmd)
				textureCompat.postTexParameter(ctx, cmd.Target, cmd.Pname, out, id, &cmd)
				return
			}

		case *GlProgramBinary:
			if !canUsePrecompiledShader(c, glDev) {
				for _, cmd := range buildStubProgram(ctx, cmd.Thread(), cmd.Extras(), s, cmd.Program) {
					t.Transform(ctx, id, cmd, out)
				}
				return
			}

		case *GlProgramBinaryOES:
			if !canUsePrecompiledShader(c, glDev) {
				for _, cmd := range buildStubProgram(ctx, cmd.Thread(), cmd.Extras(), s, cmd.Program) {
					t.Transform(ctx, id, cmd, out)
				}
				return
			}

		case *GlHint:
			if cmd.Target == GLenum_GL_GENERATE_MIPMAP_HINT && !target.supportGenerateMipmapHint {
				return // Not supported in the core profile of OpenGL.
			}

		case *GlDebugMessageCallback,
			*GlDebugMessageControl,
			*GlDebugMessageCallbackKHR,
			*GlDebugMessageControlKHR:
			// Ignore - the callback function address is invalid in replay.
			return

		case *GlGetBooleani_v,
			*GlGetBooleanv,
			*GlGetFloatv,
			*GlGetInteger64i_v,
			*GlGetInteger64v,
			*GlGetIntegeri_v,
			*GlGetIntegerv,
			*GlGetInternalformativ,
			*GlGetString,
			*GlGetStringi:
			// The acceptable values of these get functions vary between GL versions.
			// As these should have no side-effects, just drop them.
			return

		case *GlGetActiveAttrib,
			*GlGetActiveUniform:
			// The number of active attributes and uniforms can vary between compilers
			// depending on their ability to eliminate dead code. In particular,
			// dead code in pixel shader can allow code removal in the vertex shader.
			// As these should have no side-effects, just drop them.
			return

		case *GlGetProgramInterfaceiv:
			// Introduced as core in 4.3, but macOS caps out at 4.1.
			// As this should have no side-effects, just drop them.
			return

		case *GlLabelObjectEXT,
			*GlGetObjectLabelEXT,
			*GlObjectLabel,
			*GlObjectLabelKHR,
			*GlGetObjectLabel,
			*GlObjectPtrLabel,
			*GlGetObjectPtrLabel,
			*GlGetObjectLabelKHR:
			// These methods require non-trivial remapping for replay.
			// As they do not affect rendering output, just drop them.
			return

		case *GlInsertEventMarkerEXT,
			*GlPushGroupMarkerEXT,
			*GlPopGroupMarkerEXT,
			*GlPushDebugGroup,
			*GlPopDebugGroup,
			*GlPushDebugGroupKHR,
			*GlPopDebugGroupKHR,
			*GlDebugMessageInsertKHR:
			// Debug markers may not be supported on the replay device.
			// As they do not affect rendering output, just drop them.
			return

		case *GlGetProgramBinary,
			*GlGetProgramBinaryOES:
			// Program binaries are very driver specific. This command may fail on replay
			// because one of the arguments must be GL_PROGRAM_BINARY_LENGTH.
			// It has no side effects, so just drop it.
			return

		case *GlEnable:
			if cmd.Capability == GLenum_GL_FRAMEBUFFER_SRGB &&
				target.framebufferSrgb == required && contexts[c].framebufferSrgb != required &&
				c.Bound.DrawFramebuffer.GetID() == 0 {
				// Ignore enabling of FRAMEBUFFER_SRGB if the capture device did not
				// support an SRGB default framebuffer, but the replay device does. This
				// is only done if the current bound draw framebuffer is the default
				// framebuffer. The state is mutated so that when a non-default
				// framebuffer is bound later on, FRAMEBUFFER_SRGB will be enabled.
				// (see GlBindFramebuffer below)
				cmd.Mutate(ctx, s, nil /* no builder, just mutate */)
				return
			}

		case *GlDisable:
			// GL_QCOM_alpha_test adds back GL_ALPHA_TEST from GLES 1.0 as extension.
			// It seems that applications only disable it to make sure it is off, so
			// we can safely ignore it. We should not ignore glEnable for it though.
			if cmd.Capability == GLenum_GL_ALPHA_TEST_QCOM {
				return
			}

		case *GlGetGraphicsResetStatusEXT:
			// From extension GL_EXT_robustness
			// It may not be implemented by the replay driver.
			// It has no effect on rendering so just drop it.
			return

		case *GlInvalidateFramebuffer,
			*GlDiscardFramebufferEXT: // GL_EXT_discard_framebuffer
			// It may not be implemented by the replay driver.
			// It is only a hint so we can just drop it.
			// TODO: It has performance impact so we should not ignore it when profiling.
			return

		case *GlMapBufferOES:
			if !version.IsES { // Remove extension suffix on desktop.
				cmd := cb.GlMapBuffer(cmd.Target, cmd.Access, memory.Pointer(cmd.Result))
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		case *GlMapBufferRangeEXT:
			if !version.IsES { // Remove extension suffix on desktop.
				cmd := cb.GlMapBufferRange(cmd.Target, cmd.Offset, cmd.Length, cmd.Access, memory.Pointer(cmd.Result))
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		case *GlFlushMappedBufferRangeEXT:
			if !version.IsES { // Remove extension suffix on desktop.
				extras := cmd.extras
				cmd := cb.GlFlushMappedBufferRange(cmd.Target, cmd.Offset, cmd.Length)
				cmd.extras = extras
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		case *GlUnmapBufferOES:
			if !version.IsES { // Remove extension suffix on desktop.
				extras := cmd.extras
				cmd := cb.GlUnmapBuffer(cmd.Target, cmd.Result)
				cmd.extras = extras
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		case *GlBindFramebuffer:
			if target.framebufferSrgb == required && contexts[c].framebufferSrgb != required &&
				c.Pixel.FramebufferSrgb != 0 {
				// Replay device defaults FRAMEBUFFER_SRGB to disabled and allows
				// enabling it (desktop), while the capture device defaulted to enabled
				// and may or may not have allowed it to be changed (GLES). While at the
				// same time, we currently assume that the default frame buffer is not
				// SRGB capable. Thus, when SRGB is enabled in the state, and we're
				// binding the default framebuffer, SRGB needs to be disabled, and
				// specifically enabled when binding the non-default framebuffer.
				// (If it was explicetly disabled in the capture, no change is needed.)
				// TODO: Handle the use of the EGL KHR_gl_colorspace extension.
				if cmd.Target == GLenum_GL_FRAMEBUFFER || cmd.Target == GLenum_GL_DRAW_FRAMEBUFFER {
					origSrgb := c.Pixel.FramebufferSrgb
					if cmd.Framebuffer == 0 {
						out.MutateAndWrite(ctx, dID, cb.GlDisable(GLenum_GL_FRAMEBUFFER_SRGB))
					} else {
						out.MutateAndWrite(ctx, dID, cb.GlEnable(GLenum_GL_FRAMEBUFFER_SRGB))
					}
					// Change the replay driver state, but keep our mutated state,
					// so we know what to do the next time we see glBindFramebuffer.
					// TODO: Handle SRGB better.
					c.Pixel.FramebufferSrgb = origSrgb
				}
			}

		case *EglDestroyContext:
			// Removing the context would interfere with the EGLImage compat below,
			// since TextureID remapping relies on being able to find the Context by ID.
			return

		case *EglCreateImageKHR:
			{
				out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
					return cmd.Mutate(ctx, s, nil) // do not call, just mutate
				}))

				// Create GL texture as compat replacement of the EGL image
				switch cmd.Target {
				case EGLenum_EGL_GL_TEXTURE_2D:
					{
						// The mutate sets the target fileds
					}
				case EGLenum_EGL_NATIVE_BUFFER_ANDROID:
					{
						texId := newTexture(id, cb, out)
						t := newTweaker(out, dID, cb)
						defer t.revert(ctx)
						t.glBindTexture_2D(ctx, texId)
						img := GetState(s).EGLImages[cmd.Result].Image
						sizedFormat := img.SizedFormat // Might be RGB565 which is not supported on desktop
						textureCompat.convertFormat(ctx, GLenum_GL_TEXTURE_2D, &sizedFormat, nil, nil, out, id, cmd)
						out.MutateAndWrite(ctx, dID, cb.GlTexImage2D(GLenum_GL_TEXTURE_2D, 0, GLint(sizedFormat), img.Width, img.Height, 0, img.DataFormat, img.DataType, memory.Nullptr))

						out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
							GetState(s).EGLImages[cmd.Result].TargetContext = c.Identifier
							GetState(s).EGLImages[cmd.Result].TargetTexture = texId
							return nil
						}))
					}
				}
				return
			}

		case *GlEGLImageTargetTexture2DOES:
			{
				cmd := *cmd
				convertTexTarget(&cmd.Target)
				out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
					return cmd.Mutate(ctx, s, nil) // do not call, just mutate
				}))

				// Rebind the currently bound 2D texture.  This might seem like a no-op, however,
				// the remapping layer will use the ID of the EGL image replacement texture now.
				out.MutateAndWrite(ctx, dID, cb.GlBindTexture(GLenum_GL_TEXTURE_2D, c.Bound.TextureUnit.Binding2d.ID))
				return
			}

		// EXT_multisampled_render_to_texture
		case *GlRenderbufferStorageMultisampleEXT:
			{
				// TODO: Support multi-sample rendering.
				cmd := cb.GlRenderbufferStorage(cmd.Target, cmd.Internalformat, cmd.Width, cmd.Height)
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		// EXT_multisampled_render_to_texture
		case *GlFramebufferTexture2DMultisampleEXT:
			{
				// TODO: Support multi-sample rendering.
				cmd := cb.GlFramebufferTexture2D(cmd.Target, cmd.Attachment, cmd.Textarget, cmd.Texture, cmd.Level)
				out.MutateAndWrite(ctx, id, cmd)
				return
			}

		case *GlFramebufferTextureMultiviewOVR:
			{
				cmd.Mutate(ctx, s, nil /* no builder, just mutate */)
				return
			}

		case *GlLinkProgram:
			{
				out.MutateAndWrite(ctx, id, cmd)
				// Forcefully get all uniform locations, so that we can remap for applications that
				// just assume locations (in particular, apps tend to assume arrays are consecutive)
				// TODO: We should warn the developers that the consecutive layout is not guaranteed.
				prog := c.Objects.Shared.Programs[cmd.Program]
				for _, uniformIndex := range prog.ActiveUniforms.KeysSorted() {
					uniform := prog.ActiveUniforms[uniformIndex]
					for i := 0; i < int(uniform.ArraySize); i++ {
						name := fmt.Sprintf("%v[%v]", strings.TrimSuffix(uniform.Name, "[0]"), i)
						loc := uniform.Location + UniformLocation(i) // TODO: Does not have to be consecutive
						out.MutateAndWrite(ctx, dID, cb.GlGetUniformLocation(cmd.Program, name, loc))
					}
				}
				return
			}

		case *GlStartTilingQCOM, *GlEndTilingQCOM,
			*EglCreateNativeClientBufferANDROID:
			if !version.IsES {
				// This extension is not applicable on desktop.
				return
			}

		default:
			if cmd.CmdFlags().IsClear() {
				MultiviewDraw(ctx, id, cmd, out)
				return
			}
			if cmd.CmdFlags().IsDrawCall() {
				if clientVAsBound(c, clientVAs) {
					log.W(ctx, "Draw call with client-pointers not handled by the compatability layer. Command: %v", cmd)
				}
				MultiviewDraw(ctx, id, cmd, out)
				return
			}
		}

		out.MutateAndWrite(ctx, id, cmd)
	})

	return t, nil
}

// Naive multiview implementation - invoke each draw call several times with different layers
func MultiviewDraw(ctx context.Context, id api.CmdID, cmd api.Cmd, out transform.Writer) {
	s := out.State()
	c := GetContext(s, cmd.Thread())
	dID := id.Derived()
	cb := CommandBuilder{Thread: cmd.Thread()}
	numViews := uint32(1)
	c.Bound.DrawFramebuffer.ForEachAttachment(func(name GLenum, att FramebufferAttachment) {
		numViews = u32.Max(numViews, uint32(att.NumViews))
	})
	if numViews > 1 {
		for viewID := GLuint(0); viewID < GLuint(numViews); viewID++ {
			// Set the magic uniform which shaders use to fetch view-dependent attributes.
			// It is missing from the observed extras, so normal mutation would fail.
			out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
				if c.Bound.Program != nil {
					viewIDLocation := UniformLocation(0x7FFF0000)
					cb.GlGetUniformLocation(c.Bound.Program.ID, "gapid_gl_ViewID_OVR", viewIDLocation).Call(ctx, s, b)
					cb.GlUniform1ui(viewIDLocation, viewID).Call(ctx, s, b)
				}
				return nil
			}))

			// For each attachment, bind the layer corresponding to this ViewID.
			// Do not modify the state so that we do not revert to single-view for next draw call.
			c.Bound.DrawFramebuffer.ForEachAttachment(func(name GLenum, a FramebufferAttachment) {
				out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
					if a.Texture != nil {
						cb.GlFramebufferTextureLayer(GLenum_GL_DRAW_FRAMEBUFFER, name, a.Texture.ID, a.TextureLevel, a.TextureLayer+GLint(viewID)).Call(ctx, s, b)
					}
					return nil
				}))
			})
			out.MutateAndWrite(ctx, id, cmd)
		}
	} else {
		out.MutateAndWrite(ctx, id, cmd)
	}
}

func (fb *Framebuffer) ForEachAttachment(action func(GLenum, FramebufferAttachment)) {
	for i, a := range fb.ColorAttachments {
		action(GLenum_GL_COLOR_ATTACHMENT0+GLenum(i), a)
	}
	action(GLenum_GL_DEPTH_ATTACHMENT, fb.DepthAttachment)
	action(GLenum_GL_STENCIL_ATTACHMENT, fb.StencilAttachment)
}

// canUsePrecompiledShader returns true if precompiled shaders / programs
// captured with the context c can be replayed on the device d.
func canUsePrecompiledShader(c *Context, d *device.OpenGLDriver) bool {
	return c.Constants.Vendor == d.Vendor && c.Constants.Version == d.Version
}

// clientVAsBound returns true if there are any vertex attribute arrays enabled
// with pointers to client-side memory.
func clientVAsBound(c *Context, clientVAs map[*VertexAttributeArray]*GlVertexAttribPointer) bool {
	for _, arr := range c.Bound.VertexArray.VertexAttributeArrays {
		if arr.Enabled == GLboolean_GL_TRUE {
			if _, ok := clientVAs[arr]; ok {
				return true
			}
		}
	}
	return false
}

// moveClientVBsToVAs is a compatability helper for transforming client-side
// vertex array data (which is not supported by glVertexAttribPointer in later
// versions of GL), into array-buffers.
func moveClientVBsToVAs(
	ctx context.Context,
	t *tweaker,
	clientVAs map[*VertexAttributeArray]*GlVertexAttribPointer,
	first, count uint32, // vertex indices
	id api.CmdID,
	cmd api.Cmd,
	s *api.State,
	c *Context,
	out transform.Writer) {

	if count == 0 {
		return
	}

	cb := CommandBuilder{Thread: cmd.Thread()}
	rngs := interval.U64RangeList{}
	// Gather together all the client-buffers in use by the vertex-attribs.
	// Merge together all the memory intervals that these use.
	va := c.Bound.VertexArray
	for _, arr := range va.VertexAttributeArrays {
		if arr.Enabled == GLboolean_GL_TRUE {
			vb := va.VertexBufferBindings[arr.Binding]
			if cmd, ok := clientVAs[arr]; ok {
				// TODO: We're currently ignoring the Offset and Stride fields of the VBB.
				// TODO: We're currently ignoring the RelativeOffset field of the VA.
				// TODO: Merge logic with ReadVertexArrays macro in vertex_arrays.api.
				if vb.Divisor != 0 {
					panic("Instanced draw calls not currently supported by the compatibility layer")
				}
				stride, size := int(cmd.Stride), DataTypeSize(cmd.Type)*int(cmd.Size)
				if stride == 0 {
					stride = size
				}
				rng := memory.Range{
					Base: cmd.Data.addr, // Always start from the 0'th vertex to simplify logic.
					Size: uint64(int(first+count-1)*stride + size),
				}
				interval.Merge(&rngs, rng.Span(), true)
			}
		}
	}

	// Create an array-buffer for each chunk of overlapping client-side buffers in
	// use. These are populated with data below.
	ids := make([]BufferId, len(rngs))
	for i := range rngs {
		ids[i] = t.glGenBuffer(ctx)
	}

	// Apply the memory observations that were made by the draw call now.
	// We need to do this as the glBufferData calls below will require the data.
	dID := id.Derived()
	out.MutateAndWrite(ctx, dID, replay.Custom(func(ctx context.Context, s *api.State, b *builder.Builder) error {
		cmd.Extras().Observations().ApplyReads(s.Memory[memory.ApplicationPool])
		return nil
	}))

	// Note: be careful of overwriting the observations made above, before the
	// calls to glBufferData below.

	// Fill the array-buffers with the observed memory data.
	for i, rng := range rngs {
		base := memory.BytePtr(rng.First, memory.ApplicationPool)
		size := GLsizeiptr(rng.Count)
		t.GlBindBuffer_ArrayBuffer(ctx, ids[i])
		out.MutateAndWrite(ctx, dID, cb.GlBufferData(GLenum_GL_ARRAY_BUFFER, size, base, GLenum_GL_STATIC_DRAW))
	}

	// Redirect all the vertex attrib arrays to point to the array-buffer data.
	for _, l := range va.VertexAttributeArrays.KeysSorted() {
		arr := va.VertexAttributeArrays[l]
		if arr.Enabled == GLboolean_GL_TRUE {
			if cmd, ok := clientVAs[arr]; ok {
				cmd := *cmd // Copy
				i := interval.IndexOf(&rngs, cmd.Data.addr)
				t.GlBindBuffer_ArrayBuffer(ctx, ids[i])
				cmd.Data = VertexPointer{cmd.Data.addr - rngs[i].First, memory.ApplicationPool} // Offset
				out.MutateAndWrite(ctx, dID, &cmd)
			}
		}
	}
}
