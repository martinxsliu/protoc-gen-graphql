package mapper

import (
	"fmt"
	"strings"

	"github.com/golang/protobuf/proto"
	pb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/golang/protobuf/protoc-gen-go/generator"
	"github.com/martinxsliu/protoc-gen-graphql/descriptor"
	"github.com/martinxsliu/protoc-gen-graphql/graphql"
	"github.com/martinxsliu/protoc-gen-graphql/graphqlpb"
)

type Mapper struct {
	FilePbs []*pb.FileDescriptorProto
	Params  *Parameters

	// Maps file names to descriptors.
	Files map[string]*descriptor.File
	// Maps protobuf types to descriptors.
	Messages map[string]*descriptor.Message
	Enums    map[string]*descriptor.Enum

	// Maps protobuf messages and enums to graphql type names.
	ObjectNames map[string]string
	InputNames  map[string]string

	// Maps protobuf types to graphql types.
	MessageMappers map[string]*MessageMapper
	EnumMappers    map[string]*EnumMapper
	ServiceMappers map[string]*ServiceMapper
}

type MessageMapper struct {
	Descriptor *descriptor.Message
	Empty      bool
	Object     *graphql.Object
	Input      *graphql.Input
	Oneofs     []*OneofMapper
}

type OneofMapper struct {
	Descriptor *descriptor.Oneof
	Union      *graphql.Union
	Objects    []*graphql.Object
	Input      *graphql.Input
}

type EnumMapper struct {
	Descriptor *descriptor.Enum
	Enum       *graphql.Enum
}

type ServiceMapper struct {
	Descriptor    *descriptor.Service
	Queries       *graphql.Object
	Mutations     *graphql.Object
	Subscriptions *graphql.Object
}

// New creates a new Mapper with all mappings populated from the provided file
// descriptors. The provided file descriptors must be in topological order.
func New(filePbs []*pb.FileDescriptorProto, params *Parameters) *Mapper {
	m := &Mapper{
		FilePbs: filePbs,
		Params:  params,

		Files:    make(map[string]*descriptor.File),
		Messages: make(map[string]*descriptor.Message),
		Enums:    make(map[string]*descriptor.Enum),

		ObjectNames: make(map[string]string),
		InputNames:  make(map[string]string),

		MessageMappers: make(map[string]*MessageMapper),
		EnumMappers:    make(map[string]*EnumMapper),
		ServiceMappers: make(map[string]*ServiceMapper),
	}
	m.buildDescriptorMaps()
	m.buildTypeMaps()
	m.buildMappers()
	return m
}

func (m *Mapper) buildDescriptorMaps() {
	for _, filePb := range m.FilePbs {
		file := descriptor.WrapFile(filePb)
		m.Files[filePb.GetName()] = file
		for _, enum := range file.Enums {
			m.Enums[enum.FullName] = enum
		}
		for _, message := range file.Messages {
			m.Messages[message.FullName] = message
		}
	}
}

func (m *Mapper) buildTypeMaps() {
	for _, filePb := range m.FilePbs {
		file := m.Files[filePb.GetName()]
		for _, enum := range file.Enums {
			m.ObjectNames[enum.FullName] = BuildGraphqlTypeName(&GraphqlTypeNameParts{
				Package:  enum.Package,
				TypeName: enum.TypeName,
			})
		}

		for _, message := range file.Messages {
			m.buildMessageTypeMaps(message, false)
			m.buildMessageTypeMaps(message, true)
		}
	}
}

func (m *Mapper) buildMessageTypeMaps(message *descriptor.Message, input bool) {
	nameMap := m.ObjectNames
	if input {
		nameMap = m.InputNames
	}

	if nameMap[message.FullName] != "" {
		return
	}

	nameMap[message.FullName] = BuildGraphqlTypeName(&GraphqlTypeNameParts{
		Package:    message.Package,
		TypeName:   message.TypeName,
		Input:      input,
		IsProtoMap: message.IsMap,
	})

	for _, field := range message.Proto.GetField() {
		if field.GetType() == pb.FieldDescriptorProto_TYPE_MESSAGE {
			m.buildMessageTypeMaps(m.Messages[field.GetTypeName()], input)
		}
	}
}

func (m *Mapper) buildMappers() {
	for _, filePb := range m.FilePbs {
		file := m.Files[filePb.GetName()]

		// Build enum mapper first as it has no dependencies.
		for _, enum := range file.Enums {
			m.buildEnumMapper(enum)
		}
		for _, message := range file.Messages {
			m.buildMessageMapper(message, false)
		}

		if m.Params.InputMode == InputModeAll {
			for _, message := range file.Messages {
				m.buildMessageMapper(message, true)
			}
		}

		for _, service := range file.Services {
			if m.Params.InputMode == InputModeService {
				for _, method := range service.Proto.GetMethod() {
					m.buildMessageMapper(m.Messages[method.GetInputType()], true)
				}
			}

			// Build service mapper last, after all dependencies are mapped.
			if m.Params.InputMode != InputModeNone {
				m.buildServiceMapper(service)
			}
		}
	}
}

// Do not call buildMessageMapper with the same message and input=false
// after calling it with input=true, otherwise the input objects for
// the oneofs will be overwritten.
func (m *Mapper) buildMessageMapper(message *descriptor.Message, input bool) {
	mapper, ok := m.MessageMappers[message.FullName]
	if ok {
		if (input && mapper.Input != nil) || (!input && mapper.Object != nil) {
			return
		}
	}

	if !ok {
		mapper = &MessageMapper{Descriptor: message}
		m.MessageMappers[message.FullName] = mapper
	}

	if len(message.Fields) == 0 {
		mapper.Empty = true
	}

	mapper.Object = &graphql.Object{
		Name:   m.ObjectNames[message.FullName],
		Fields: m.graphqlFields(message, false),
	}
	if input {
		mapper.Input = &graphql.Input{
			Name:   m.InputNames[message.FullName],
			Fields: m.graphqlFields(message, true),
		}
	}

	var oneofMappers []*OneofMapper
	for _, oneof := range message.Oneofs {
		oneofMappers = append(oneofMappers, m.buildOneofMapper(oneof, input))
	}
	mapper.Oneofs = oneofMappers

	for _, field := range message.Proto.GetField() {
		if field.GetType() == pb.FieldDescriptorProto_TYPE_MESSAGE {
			m.buildMessageMapper(m.Messages[field.GetTypeName()], input)
		}
	}
}

func (m *Mapper) graphqlFields(message *descriptor.Message, input bool) []*graphql.Field {
	var fields []*graphql.Field

	if len(message.Fields) == 0 {
		fields = append(fields, &graphql.Field{
			Name:     "_empty",
			TypeName: graphql.ScalarBoolean.TypeName(),
		})
		return fields
	}

	for _, field := range message.Fields {
		if field.IsOneof {
			oneofObjectName := field.Name + "Oneof"
			fields = append(fields, &graphql.Field{
				Name: field.Name,
				TypeName: BuildGraphqlTypeName(&GraphqlTypeNameParts{
					Package:  message.Package,
					TypeName: append(message.TypeName, oneofObjectName),
					Input:    input,
				}),
			})
			continue
		}

		fields = append(fields, m.graphqlField(field.Proto, fieldOptions{Input: input}))
	}
	return fields
}

type fieldOptions struct {
	Input           bool
	NullableScalars bool
}

func (m *Mapper) graphqlField(proto *pb.FieldDescriptorProto, options fieldOptions) *graphql.Field {
	field := &graphql.Field{
		Name: proto.GetName(),
	}

	switch proto.GetType() {
	case pb.FieldDescriptorProto_TYPE_STRING, pb.FieldDescriptorProto_TYPE_BYTES:
		field.TypeName = graphql.ScalarString.TypeName()
		if !options.NullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_FLOAT, pb.FieldDescriptorProto_TYPE_DOUBLE,
		pb.FieldDescriptorProto_TYPE_INT32, pb.FieldDescriptorProto_TYPE_UINT32, pb.FieldDescriptorProto_TYPE_SINT32,
		pb.FieldDescriptorProto_TYPE_FIXED32, pb.FieldDescriptorProto_TYPE_SFIXED32:

		field.TypeName = graphql.ScalarFloat.TypeName()
		if !options.NullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_INT64, pb.FieldDescriptorProto_TYPE_UINT64, pb.FieldDescriptorProto_TYPE_SINT64,
		pb.FieldDescriptorProto_TYPE_FIXED64, pb.FieldDescriptorProto_TYPE_SFIXED64:

		if m.Params.String64Bit {
			field.TypeName = graphql.ScalarString.TypeName()
		} else {
			field.TypeName = graphql.ScalarFloat.TypeName()
		}
		if !options.NullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_BOOL:
		field.TypeName = graphql.ScalarBoolean.TypeName()
		if !options.NullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_ENUM:
		field.TypeName = m.EnumMappers[proto.GetTypeName()].Enum.Name
		if !options.NullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_MESSAGE:
		if options.Input {
			field.TypeName = m.InputNames[proto.GetTypeName()]
		} else {
			field.TypeName = m.ObjectNames[proto.GetTypeName()]
		}

		// IsProtoMap elements are non-nullable.
		if m.Messages[proto.GetTypeName()].IsMap {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	default:
		panic(fmt.Sprintf("unexpected protobuf descriptor type: %s", proto.GetType().String()))
	}

	if proto.GetLabel() == pb.FieldDescriptorProto_LABEL_REPEATED {
		field.Modifiers = field.Modifiers | graphql.TypeModifierNonNull | graphql.TypeModifierList
	}

	return m.graphqlSpecialTypes(field, proto.GetTypeName())
}

func (m *Mapper) graphqlSpecialTypes(field *graphql.Field, protoTypeName string) *graphql.Field {
	if protoTypeName == ".google.protobuf.Timestamp" && m.Params.TimestampTypeName != "" {
		field.TypeName = m.Params.TimestampTypeName
	}
	if protoTypeName == ".google.protobuf.Duration" && m.Params.DurationTypeName != "" {
		field.TypeName = m.Params.DurationTypeName
	}

	if m.Params.WrappersAsNull {
		switch protoTypeName {
		case ".google.protobuf.FloatValue", ".google.protobuf.DoubleValue", ".google.protobuf.UInt32Value":
			field.TypeName = graphql.ScalarFloat.TypeName()
		case ".google.protobuf.StringValue", ".google.protobuf.BytesValue", ".google.protobuf.Int64Value", ".google.protobuf.UInt64Value":
			field.TypeName = graphql.ScalarString.TypeName()
		case ".google.protobuf.Int32Value":
			field.TypeName = graphql.ScalarInt.TypeName()
		case ".google.protobuf.BoolValue":
			field.TypeName = graphql.ScalarBoolean.TypeName()
		}
	}

	return field
}

func (m *Mapper) buildOneofMapper(oneof *descriptor.Oneof, input bool) *OneofMapper {
	oneofObjectName := oneof.Proto.GetName() + "Oneof"
	mapper := &OneofMapper{
		Descriptor: oneof,
		Union: &graphql.Union{
			Name: BuildGraphqlTypeName(&GraphqlTypeNameParts{
				Package:  oneof.Parent.Package,
				TypeName: append(oneof.Parent.TypeName, oneofObjectName),
			}),
		},
	}

	for _, fieldProto := range oneof.FieldProtos {
		typeName := BuildGraphqlTypeName(&GraphqlTypeNameParts{
			Package:  oneof.Parent.Package,
			TypeName: append(oneof.Parent.TypeName, oneofObjectName, fieldProto.GetName()),
		})

		mapper.Union.TypeNames = append(mapper.Union.TypeNames, typeName)
		mapper.Objects = append(mapper.Objects, &graphql.Object{
			Name: typeName,
			Fields: []*graphql.Field{
				// Include typename so we can differentiate between messages in a oneof.
				&graphql.Field{
					Name:     "_typename",
					TypeName: graphql.ScalarString.TypeName(),
				},
				m.graphqlField(fieldProto, fieldOptions{}),
			},
		})
	}

	if !input {
		return mapper
	}

	var inputFields []*graphql.Field
	for _, fieldProto := range oneof.FieldProtos {
		inputFields = append(inputFields, m.graphqlField(fieldProto, fieldOptions{Input: true, NullableScalars: true}))
	}

	mapper.Input = &graphql.Input{
		Name: BuildGraphqlTypeName(&GraphqlTypeNameParts{
			Package:  oneof.Parent.Package,
			TypeName: append(oneof.Parent.TypeName, oneofObjectName),
			Input:    true,
		}),
		Fields: inputFields,
	}

	return mapper
}

func (m *Mapper) buildEnumMapper(enum *descriptor.Enum) {
	var values []string
	for _, protoValue := range enum.Proto.GetValue() {
		values = append(values, protoValue.GetName())
	}

	m.EnumMappers[enum.FullName] = &EnumMapper{
		Descriptor: enum,
		Enum: &graphql.Enum{
			Name:   m.ObjectNames[enum.FullName],
			Values: values,
		},
	}
}

func (m *Mapper) buildServiceMapper(service *descriptor.Service) {
	var (
		queries       []*graphql.Field
		mutations     []*graphql.Field
		subscriptions []*graphql.Field
	)

	for _, method := range service.Proto.GetMethod() {
		var operation string
		if proto.HasExtension(method.GetOptions(), graphqlpb.E_Operation) {
			extVal, err := proto.GetExtension(method.GetOptions(), graphqlpb.E_Operation)
			if err != nil {
				panic(err)
			}
			operation = *extVal.(*string)
		}
		if operation == "none" {
			return
		}

		field := m.graphqlFieldFromMethod(method)

		switch operation {
		case "mutation":
			mutations = append(mutations, field)
		case "subscription":
			subscriptions = append(subscriptions, field)
		default:
			queries = append(queries, field)
		}
	}

	mapper := &ServiceMapper{
		Descriptor: service,
	}
	if len(queries) > 0 {
		mapper.Queries = &graphql.Object{
			Name: BuildGraphqlTypeName(&GraphqlTypeNameParts{
				Package:  service.Package,
				TypeName: append(service.TypeName, "Query"),
			}),
			Fields: queries,
		}
	}
	if len(mutations) > 0 {
		mapper.Mutations = &graphql.Object{
			Name: BuildGraphqlTypeName(&GraphqlTypeNameParts{
				Package:  service.Package,
				TypeName: append(service.TypeName, "Mutation"),
			}),
			Fields: mutations,
		}
	}
	if len(subscriptions) > 0 {
		mapper.Subscriptions = &graphql.Object{
			Name: BuildGraphqlTypeName(&GraphqlTypeNameParts{
				Package:  service.Package,
				TypeName: append(service.TypeName, "Subscription"),
			}),
			Fields: subscriptions,
		}
	}

	m.ServiceMappers[service.FullName] = mapper
}

func (m *Mapper) graphqlFieldFromMethod(method *pb.MethodDescriptorProto) *graphql.Field {
	// Only add an argument if there are fields in the gRPC request message.
	var arguments []*graphql.Argument
	inputType := m.Messages[method.GetInputType()]
	if len(inputType.Fields) != 0 {
		arguments = append(arguments, &graphql.Argument{
			Name:      "input",
			TypeName:  m.MessageMappers[method.GetInputType()].Input.Name,
			Modifiers: graphql.TypeModifierNonNull,
		})
	}

	// If the response message has no fields then return a nullable Boolean.
	// It is up to the resolver's implementation whether or not to return an
	// actual boolean value or default to null.
	outputType := m.Messages[method.GetOutputType()]
	if len(outputType.Fields) == 0 {
		return &graphql.Field{
			Name:      method.GetName(),
			TypeName:  graphql.ScalarBoolean.TypeName(),
			Arguments: arguments,
		}
	}

	return &graphql.Field{
		Name:      method.GetName(),
		TypeName:  m.MessageMappers[method.GetOutputType()].Object.Name,
		Arguments: arguments,
		Modifiers: graphql.TypeModifierNonNull,
	}
}

type GraphqlTypeNameParts struct {
	Package    string
	TypeName   []string
	IsProtoMap bool
	Input      bool
}

func BuildGraphqlTypeName(parts *GraphqlTypeNameParts) string {
	var b strings.Builder
	b.WriteString(generator.CamelCaseSlice(strings.Split(parts.Package, ".")))
	for i, name := range parts.TypeName {
		if parts.IsProtoMap && i == len(parts.TypeName)-1 {
			name = strings.TrimSuffix(name, "Entry")
		}

		b.WriteString("_")
		b.WriteString(generator.CamelCase(name))
	}
	if parts.IsProtoMap {
		b.WriteString("Map")
	}
	if parts.Input {
		b.WriteString("Input")
	}

	return b.String()
}